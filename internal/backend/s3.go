package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

type s3Backend struct {
	client    *s3.Client
	presigner *s3.PresignClient
}

func newS3Backend(cfg S3Config) (Backend, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	var endpointOpts []func(*s3.Options)
	if cfg.Endpoint != "" {
		ep := cfg.Endpoint
		pathStyle := cfg.ForcePathStyle
		endpointOpts = append(endpointOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ep)
			o.UsePathStyle = pathStyle
		})
	}

	client := s3.NewFromConfig(awsCfg, endpointOpts...)
	return &s3Backend{
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func (b *s3Backend) HeadObject(ctx context.Context, in HeadObjectInput) (*HeadObjectOutput, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	})
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &HeadObjectOutput{
		ContentLength: aws.ToInt64(out.ContentLength),
		ContentType:   aws.ToString(out.ContentType),
		ETag:          stripQuotes(aws.ToString(out.ETag)),
		LastModified:  aws.ToTime(out.LastModified),
		Metadata:      out.Metadata,
	}, nil
}

func (b *s3Backend) GetObject(ctx context.Context, in GetObjectInput) (*GetObjectOutput, error) {
	req := &s3.GetObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	}
	if in.Range != "" {
		req.Range = aws.String(in.Range)
	}

	out, err := b.client.GetObject(ctx, req)
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &GetObjectOutput{
		Body:          out.Body,
		ContentLength: aws.ToInt64(out.ContentLength),
		ContentRange:  aws.ToString(out.ContentRange),
		ContentType:   aws.ToString(out.ContentType),
		ETag:          stripQuotes(aws.ToString(out.ETag)),
		LastModified:  aws.ToTime(out.LastModified),
		Metadata:      out.Metadata,
	}, nil
}

func (b *s3Backend) PutObject(ctx context.Context, in PutObjectInput) (*PutObjectOutput, error) {
	req := &s3.PutObjectInput{
		Bucket:   aws.String(in.Bucket),
		Key:      aws.String(in.Key),
		Body:     in.Body,
		Metadata: in.Metadata,
	}
	if in.ContentType != "" {
		req.ContentType = aws.String(in.ContentType)
	}
	if in.Size >= 0 {
		req.ContentLength = aws.Int64(in.Size)
	}

	out, err := b.client.PutObject(ctx, req)
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &PutObjectOutput{ETag: stripQuotes(aws.ToString(out.ETag))}, nil
}

func (b *s3Backend) DeleteObject(ctx context.Context, in DeleteObjectInput) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(in.Bucket),
		Key:    aws.String(in.Key),
	})
	return mapS3Err(err)
}

func (b *s3Backend) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsOutput, error) {
	ids := make([]types.ObjectIdentifier, len(in.Objects))
	for i, o := range in.Objects {
		ids[i] = types.ObjectIdentifier{Key: aws.String(o.Key)}
	}

	out, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(in.Bucket),
		Delete: &types.Delete{
			Objects: ids,
			Quiet:   aws.Bool(in.Quiet),
		},
	})
	if err != nil {
		return nil, mapS3Err(err)
	}

	result := &DeleteObjectsOutput{}
	for _, d := range out.Deleted {
		result.Deleted = append(result.Deleted, DeletedObject{Key: aws.ToString(d.Key)})
	}
	for _, e := range out.Errors {
		result.Errors = append(result.Errors, DeleteError{
			Key:     aws.ToString(e.Key),
			Code:    aws.ToString(e.Code),
			Message: aws.ToString(e.Message),
		})
	}
	return result, nil
}

func (b *s3Backend) HeadBucket(ctx context.Context, in HeadBucketInput) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(in.Bucket),
	})
	return mapS3Err(err)
}

func (b *s3Backend) ListObjects(ctx context.Context, in ListObjectsInput) (*ListObjectsOutput, error) {
	req := &s3.ListObjectsV2Input{
		Bucket: aws.String(in.Bucket),
	}
	if in.Prefix != "" {
		req.Prefix = aws.String(in.Prefix)
	}
	if in.Delimiter != "" {
		req.Delimiter = aws.String(in.Delimiter)
	}
	if in.MaxKeys > 0 {
		req.MaxKeys = aws.Int32(in.MaxKeys)
	}
	if in.StartAfter != "" {
		req.StartAfter = aws.String(in.StartAfter)
	}
	if in.ContinuationToken != "" {
		req.ContinuationToken = aws.String(in.ContinuationToken)
	}

	out, err := b.client.ListObjectsV2(ctx, req)
	if err != nil {
		return nil, mapS3Err(err)
	}

	result := &ListObjectsOutput{
		IsTruncated:           aws.ToBool(out.IsTruncated),
		NextContinuationToken: aws.ToString(out.NextContinuationToken),
		KeyCount:              aws.ToInt32(out.KeyCount),
	}
	for _, obj := range out.Contents {
		result.Contents = append(result.Contents, ObjectInfo{
			Key:          aws.ToString(obj.Key),
			Size:         aws.ToInt64(obj.Size),
			ETag:         stripQuotes(aws.ToString(obj.ETag)),
			LastModified: aws.ToTime(obj.LastModified),
			StorageClass: string(obj.StorageClass),
		})
	}
	for _, cp := range out.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, aws.ToString(cp.Prefix))
	}
	return result, nil
}

func (b *s3Backend) CreateMultipartUpload(ctx context.Context, in CreateMultipartUploadInput) (*CreateMultipartUploadOutput, error) {
	req := &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(in.Bucket),
		Key:      aws.String(in.Key),
		Metadata: in.Metadata,
	}
	if in.ContentType != "" {
		req.ContentType = aws.String(in.ContentType)
	}

	out, err := b.client.CreateMultipartUpload(ctx, req)
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &CreateMultipartUploadOutput{UploadID: aws.ToString(out.UploadId)}, nil
}

func (b *s3Backend) UploadPart(ctx context.Context, in UploadPartInput) (*UploadPartOutput, error) {
	req := &s3.UploadPartInput{
		Bucket:     aws.String(in.Bucket),
		Key:        aws.String(in.Key),
		UploadId:   aws.String(in.UploadID),
		PartNumber: aws.Int32(in.PartNumber),
		Body:       in.Body,
	}
	if in.Size >= 0 {
		req.ContentLength = aws.Int64(in.Size)
	}

	out, err := b.client.UploadPart(ctx, req)
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &UploadPartOutput{ETag: stripQuotes(aws.ToString(out.ETag))}, nil
}

func (b *s3Backend) CompleteMultipartUpload(ctx context.Context, in CompleteMultipartUploadInput) (*CompleteMultipartUploadOutput, error) {
	parts := make([]types.CompletedPart, len(in.Parts))
	for i, p := range in.Parts {
		etag := p.ETag
		parts[i] = types.CompletedPart{
			PartNumber: aws.Int32(p.PartNumber),
			ETag:       aws.String(etag),
		}
	}

	out, err := b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(in.Bucket),
		Key:             aws.String(in.Key),
		UploadId:        aws.String(in.UploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return nil, mapS3Err(err)
	}
	return &CompleteMultipartUploadOutput{ETag: stripQuotes(aws.ToString(out.ETag))}, nil
}

func (b *s3Backend) AbortMultipartUpload(ctx context.Context, in AbortMultipartUploadInput) error {
	_, err := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(in.Bucket),
		Key:      aws.String(in.Key),
		UploadId: aws.String(in.UploadID),
	})
	return mapS3Err(err)
}

func (b *s3Backend) PresignURL(ctx context.Context, in PresignInput) (*PresignOutput, error) {
	var (
		url string
		err error
	)
	expiry := func(o *s3.PresignOptions) { o.Expires = in.Expires }

	switch in.Method {
	case "GET":
		req, e := b.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(in.Bucket),
			Key:    aws.String(in.Key),
		}, expiry)
		if e != nil {
			return nil, mapS3Err(e)
		}
		url = req.URL
	case "PUT":
		req, e := b.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(in.Bucket),
			Key:    aws.String(in.Key),
		}, expiry)
		if e != nil {
			return nil, mapS3Err(e)
		}
		url = req.URL
	default:
		return nil, fmt.Errorf("presign: unsupported method %q", in.Method)
	}
	return &PresignOutput{URL: url}, err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func stripQuotes(s string) string {
	return strings.Trim(s, `"`)
}

func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return ErrObjectNotFound
	}
	var noBucket *types.NoSuchBucket
	if errors.As(err, &noBucket) {
		return ErrBucketNotFound
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return ErrObjectNotFound
		case "NoSuchBucket":
			return ErrBucketNotFound
		case "AccessDenied", "Forbidden":
			return ErrAccessDenied
		case "InvalidRange":
			return fmt.Errorf("%w: %w", ErrInvalidRange, err)
		}
	}
	return fmt.Errorf("%w: %w", ErrUpstreamError, err)
}
