package downloader

// TaskOperationBinder is an optional local adapter scope. It is immutable
// after its first binding and never becomes part of a public or Node DTO.
type TaskOperationBinder interface {
	BindTaskOperation(string) error
}
