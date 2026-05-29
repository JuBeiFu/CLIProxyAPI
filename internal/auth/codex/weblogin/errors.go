package weblogin

import "errors"

// ErrAccountBanned signals a genuine account-level rejection at authorize/login —
// the caller should let the auth go terminal. Its message intentionally contains a
// terminalAuthFailureReason needle so the conductor disables/deletes the auth.
var ErrAccountBanned = errors.New("account_deactivated: login rejected at authorize")

// ErrLoginTransient signals a recoverable failure (CF 403, network, sentinel). Its
// message must NOT contain any terminalAuthFailureReason needle, so the conductor
// applies a retry backoff instead of nuking the auth.
var ErrLoginTransient = errors.New("login temporarily failed; will retry")
