package auth

import "testing"

func TestIsUsageLimitReachedError(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want bool
	}{
		{
			name: "usage_limit_reached in message",
			err:  &Error{Message: `{"error":{"type":"usage_limit_reached","resets_at":1700000000}}`},
			want: true,
		},
		{
			name: "plain rate limit",
			err:  &Error{Message: "rate limit exceeded"},
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "resets_in_seconds present",
			err:  &Error{Message: `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`},
			want: true,
		},
		{
			name: "empty message",
			err:  &Error{Message: ""},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUsageLimitReachedError(tt.err)
			if got != tt.want {
				t.Errorf("isUsageLimitReachedError() = %v, want %v", got, tt.want)
			}
		})
	}
}
