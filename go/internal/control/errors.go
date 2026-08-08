package control

import "fmt"

// Error is a client-facing control-plane error.
type Error struct {
	Status  int
	Code    string
	Message string
	// Detail carries fields a client needs in order to react, merged into the error
	// body alongside code and message. It exists because some things a caller must
	// know are only knowable from a refusal: the sign-in screen learns whether guest
	// browsing is offered from the 401 that told it to sign in, since every endpoint
	// that could have answered is behind that same check.
	//
	// Never put anything here that the caller has not already proved it may see.
	Detail map[string]any
}

func (e *Error) Error() string { return e.Message }

// NewError builds a client-facing error.
func NewError(status int, code, format string, args ...any) error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// NewErrorWithDetail builds a client-facing error carrying extra fields.
func NewErrorWithDetail(
	status int, code string, detail map[string]any, format string, args ...any,
) error {
	return &Error{
		Status: status, Code: code, Detail: detail,
		Message: fmt.Sprintf(format, args...),
	}
}
