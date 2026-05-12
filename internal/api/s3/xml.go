package s3

import (
	"encoding/xml"
	"net/http"
)

// ---------------------------------------------------------------------------
// Response types
// ---------------------------------------------------------------------------

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets struct {
		Bucket []bucketInfo `xml:"Bucket"`
	} `xml:"Buckets"`
}

type bucketInfo struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listBucketResult struct {
	XMLName               xml.Name   `xml:"ListBucketResult"`
	Name                  string     `xml:"Name"`
	Prefix                string     `xml:"Prefix"`
	Delimiter             string     `xml:"Delimiter,omitempty"`
	MaxKeys               int32      `xml:"MaxKeys"`
	KeyCount              int32      `xml:"KeyCount"`
	IsTruncated           bool       `xml:"IsTruncated"`
	ContinuationToken     string     `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	StartAfter            string     `xml:"StartAfter,omitempty"`
	Contents              []s3Object `xml:"Contents"`
	CommonPrefixes        []s3Prefix `xml:"CommonPrefixes"`
}

type s3Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type s3Prefix struct {
	Prefix string `xml:"Prefix"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

// completeMultipartUploadRequest is the XML body sent by the client.
type completeMultipartUploadRequest struct {
	Parts []completedPartXML `xml:"Part"`
}

type completedPartXML struct {
	PartNumber int32  `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// deleteRequest is the XML body for POST /{bucket}?delete.
type deleteRequest struct {
	Quiet   bool              `xml:"Quiet"`
	Objects []deleteObjectXML `xml:"Object"`
}

type deleteObjectXML struct {
	Key string `xml:"Key"`
}

type deleteResult struct {
	XMLName xml.Name         `xml:"DeleteResult"`
	Deleted []deletedXML     `xml:"Deleted"`
	Error   []deleteErrorXML `xml:"Error"`
}

type deletedXML struct {
	Key string `xml:"Key"`
}

type deleteErrorXML struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type s3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId"`
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func writeS3XML(w http.ResponseWriter, statusCode int, v interface{}) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", newRequestID())
	w.WriteHeader(statusCode)
	w.Write([]byte(xml.Header)) //nolint:errcheck
	xml.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeS3Error(w http.ResponseWriter, statusCode int, code, message string) {
	writeS3XML(w, statusCode, s3ErrorResponse{
		Code:      code,
		Message:   message,
		RequestID: newRequestID(),
	})
}
