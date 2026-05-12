package registry

import "errors"

var (
	ErrAccessKeyNotFound = errors.New("access key not found or revoked")
	ErrTenantNotFound    = errors.New("tenant not found")
	ErrStoreNotFound     = errors.New("store not found")
	ErrBucketNotFound    = errors.New("gateway bucket not found")
	ErrMultipartNotFound = errors.New("multipart upload not found")
	ErrUnauthorized      = errors.New("bucket does not belong to tenant")
)
