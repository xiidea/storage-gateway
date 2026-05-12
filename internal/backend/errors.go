package backend

import "errors"

var (
	ErrObjectNotFound = errors.New("object not found")
	ErrBucketNotFound = errors.New("bucket not found")
	ErrAccessDenied   = errors.New("access denied by upstream")
	ErrUnknownSize    = errors.New("content length required but not provided")
	ErrUpstreamError  = errors.New("upstream storage error")
)

// IsNotFound reports whether err indicates a missing object or bucket.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrBucketNotFound)
}
