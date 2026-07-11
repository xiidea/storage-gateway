package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"storage-gateway/internal/auth"
	"storage-gateway/internal/registry"
)

const (
	sigV4Algorithm  = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptyHash       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type parsedAuth struct {
	accessKeyID   string
	date          string // YYYYMMDD
	region        string
	signedHeaders []string
	signature     string
	dateTime      string // YYYYMMDDTHHMMSSZ
	presigned     bool
	expires       int64 // seconds (presigned only)
}

// authMiddleware verifies AWS Sig V4 on every request (header or presigned query params).
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pa, err := parseAuth(r)
		if err != nil {
			writeS3Error(w, http.StatusForbidden, "InvalidSecurity", err.Error())
			return
		}

		keyRow, err := h.mgr.LookupAccessKey(r.Context(), pa.accessKeyID)
		if err != nil {
			if errors.Is(err, registry.ErrAccessKeyNotFound) {
				writeS3Error(w, http.StatusForbidden, "InvalidAccessKeyId", "the access key does not exist")
				return
			}
			h.log.Error("lookup access key", "err", err)
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
			return
		}

		secretKeyBytes, err := auth.Decrypt(h.cryptoKey, keyRow.SecretKeyEnc)
		if err != nil {
			h.log.Error("decrypt secret key", "key_id", pa.accessKeyID, "err", err)
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "internal error")
			return
		}

		if pa.presigned {
			dt, err := time.Parse("20060102T150405Z", pa.dateTime)
			if err != nil {
				writeS3Error(w, http.StatusForbidden, "AuthorizationQueryParametersError", "invalid X-Amz-Date")
				return
			}
			if time.Now().After(dt.Add(time.Duration(pa.expires) * time.Second)) {
				writeS3Error(w, http.StatusForbidden, "RequestExpired", "presigned URL has expired")
				return
			}
		}

		expected := computeSignature(r, pa, string(secretKeyBytes))
		if !hmac.Equal([]byte(pa.signature), []byte(expected)) {
			writeS3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", "the request signature does not match")
			return
		}

		// Readonly keys may only perform read/query operations (GET, HEAD).
		// PUT, DELETE and POST (uploads, multipart, batch delete) are denied.
		if keyRow.Readonly && r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeS3Error(w, http.StatusForbidden, "AccessDenied", "access key is read-only")
			return
		}

		ctx := context.WithValue(r.Context(), ctxTenantID, keyRow.TenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// parseAuth dispatches to header or presigned parsing based on query params.
func parseAuth(r *http.Request) (*parsedAuth, error) {
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return parsePresignedAuth(r)
	}
	return parseHeaderAuth(r)
}

// parseHeaderAuth parses: AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...
func parseHeaderAuth(r *http.Request) (*parsedAuth, error) {
	const prefix = "AWS4-HMAC-SHA256 "
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, prefix) {
		return nil, fmt.Errorf("missing or malformed Authorization header")
	}

	parts := make(map[string]string)
	for _, part := range strings.Split(authHeader[len(prefix):], ", ") {
		if i := strings.IndexByte(part, '='); i > 0 {
			parts[part[:i]] = part[i+1:]
		}
	}

	credential := parts["Credential"]
	signedHeaders := parts["SignedHeaders"]
	signature := parts["Signature"]
	if credential == "" || signedHeaders == "" || signature == "" {
		return nil, fmt.Errorf("malformed Authorization header")
	}

	creds := strings.SplitN(credential, "/", 5)
	if len(creds) != 5 || creds[3] != "s3" || creds[4] != "aws4_request" {
		return nil, fmt.Errorf("invalid Credential")
	}

	dateTime := r.Header.Get("X-Amz-Date")
	if dateTime == "" {
		return nil, fmt.Errorf("missing X-Amz-Date header")
	}

	return &parsedAuth{
		accessKeyID:   creds[0],
		date:          creds[1],
		region:        creds[2],
		signedHeaders: strings.Split(signedHeaders, ";"),
		signature:     signature,
		dateTime:      dateTime,
	}, nil
}

// parsePresignedAuth parses auth from X-Amz-* query parameters.
func parsePresignedAuth(r *http.Request) (*parsedAuth, error) {
	q := r.URL.Query()

	if q.Get("X-Amz-Algorithm") != sigV4Algorithm {
		return nil, fmt.Errorf("unsupported algorithm: %s", q.Get("X-Amz-Algorithm"))
	}

	credential := q.Get("X-Amz-Credential")
	dateTime := q.Get("X-Amz-Date")
	signedHeadersStr := q.Get("X-Amz-SignedHeaders")
	signature := q.Get("X-Amz-Signature")
	expiresStr := q.Get("X-Amz-Expires")

	if credential == "" || dateTime == "" || signedHeadersStr == "" || signature == "" || expiresStr == "" {
		return nil, fmt.Errorf("missing required presigned parameters")
	}

	creds := strings.SplitN(credential, "/", 5)
	if len(creds) != 5 || creds[3] != "s3" || creds[4] != "aws4_request" {
		return nil, fmt.Errorf("invalid X-Amz-Credential")
	}

	expires, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid X-Amz-Expires")
	}

	return &parsedAuth{
		accessKeyID:   creds[0],
		date:          creds[1],
		region:        creds[2],
		signedHeaders: strings.Split(signedHeadersStr, ";"),
		signature:     signature,
		dateTime:      dateTime,
		presigned:     true,
		expires:       expires,
	}, nil
}

func computeSignature(r *http.Request, pa *parsedAuth, secretKey string) string {
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		if pa.presigned {
			payloadHash = unsignedPayload
		} else {
			payloadHash = emptyHash
		}
	}

	canonReq := buildCanonicalRequest(r, pa, payloadHash)
	credScope := pa.date + "/" + pa.region + "/s3/aws4_request"
	strToSign := sigV4Algorithm + "\n" + pa.dateTime + "\n" + credScope + "\n" + hashHex(canonReq)
	sigKey := signingKey(secretKey, pa.date, pa.region, "s3")
	return hex.EncodeToString(hmacSHA256(sigKey, []byte(strToSign)))
}

func buildCanonicalRequest(r *http.Request, pa *parsedAuth, payloadHash string) string {
	canonURI := canonicalURI(r.URL.Path)
	var canonQS string
	if pa.presigned {
		canonQS = canonicalQueryStringExcluding(r.URL.RawQuery, "X-Amz-Signature")
	} else {
		canonQS = canonicalQueryString(r.URL.RawQuery)
	}
	canonHeaders, signedHeaders := buildCanonicalHeaders(r, pa.signedHeaders)

	return r.Method + "\n" +
		canonURI + "\n" +
		canonQS + "\n" +
		canonHeaders + "\n" +
		signedHeaders + "\n" +
		payloadHash
}

// canonicalURI URI-encodes each path segment, preserving slashes.
func canonicalURI(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	segs := strings.Split(rawPath, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, true)
	}
	return strings.Join(segs, "/")
}

func canonicalQueryString(rawQuery string) string {
	return canonicalQueryStringExcluding(rawQuery, "")
}

func canonicalQueryStringExcluding(rawQuery, exclude string) string {
	if rawQuery == "" {
		return ""
	}
	parsed, _ := url.ParseQuery(rawQuery)
	type kv struct{ k, v string }
	var pairs []kv
	for k, vals := range parsed {
		if k == exclude {
			continue
		}
		for _, v := range vals {
			pairs = append(pairs, kv{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// buildCanonicalHeaders returns the canonical headers string and the signed-headers string.
func buildCanonicalHeaders(r *http.Request, signedHeaders []string) (canonical, joined string) {
	headers := make(map[string]string, len(signedHeaders))
	for _, name := range signedHeaders {
		lower := strings.ToLower(name)
		if lower == "host" {
			headers[lower] = r.Host
		} else {
			headers[lower] = strings.TrimSpace(r.Header.Get(name))
		}
	}

	sorted := make([]string, 0, len(headers))
	for k := range headers {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var sb strings.Builder
	for _, k := range sorted {
		sb.WriteString(k)
		sb.WriteByte(':')
		sb.WriteString(headers[k])
		sb.WriteByte('\n')
	}
	return sb.String(), strings.Join(sorted, ";")
}

// uriEncode percent-encodes s per the AWS Sig V4 spec.
// When encodeSlash is true, '/' is also encoded.
func uriEncode(s string, encodeSlash bool) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~' ||
			(!encodeSlash && c == '/') {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

func signingKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
