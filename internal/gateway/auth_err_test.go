package gateway

import (
	"errors"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"http=401 msg=<html>401 Authorization Required", true},
		{"http=403 msg=forbidden", true},
		{"http=500 msg=server error", false},
		{"Post \"https://x\": EOF", false},
		{"", false},
	}
	for _, c := range cases {
		err := error(nil)
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := isAuthError(err); got != c.want {
			t.Errorf("isAuthError(%q) = %v, 期望 %v", c.msg, got, c.want)
		}
	}
	if isAuthError(nil) {
		t.Error("nil 应返回 false")
	}
}
