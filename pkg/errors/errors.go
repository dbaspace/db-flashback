package errors

import "encoding/json"

var errBadRequest = Error{Code: "GED-C-400", Msg: "bad request", Details: map[string]string{}}
var errUnknown = Error{Code: "GED-S-599", Msg: "unknown error", Details: map[string]string{}}

func ErrBadRequest() *Error {
	r := errBadRequest
	r.Details = make(map[string]string)
	return &r
}

func ErrUnknown() *Error {
	r := errUnknown
	r.Details = make(map[string]string)
	return &r
}

type Error struct {
	Code    string            `json:"code"`
	Msg     string            `json:"message"`
	Details map[string]string `json:"details"`
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) JSONString() string {
	data, err := json.Marshal(e)
	if err != nil {
		return e.Error()
	}
	return string(data)
}

func (e *Error) SetError(err error) *Error {
	if e.Details == nil {
		e.Details = make(map[string]string)
	}
	e.Details["err"] = err.Error()
	return e
}

func (e *Error) SetMsg(msg string) *Error {
	e.Msg = msg
	return e
}
