// Package classifyhost serves the trial error classifier to the kavach CLI, so
// that a misclassified error can be recorded, replayed and its fix verified
// like any other incident.
package classifyhost

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// Message is the text of the message sink: the classifier's answer.
const Message = "message"

// Request is the data of an input: the error text of a "job" or a "trial".
type Request struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// KeyRejected is the invariant a visitor's trust rests on.
const KeyRejected = "key_rejected_only_on_auth_failure"

// Classifier is the Handler: it answers each request with api.ClassifyError.
type Classifier struct {
	text, msg string
}

func (c *Classifier) Handle(env kavach.Env, in kavach.Input) error {
	var r Request
	if err := json.Unmarshal(in.Data, &r); err != nil {
		return fmt.Errorf("classifyhost: %w", err)
	}
	c.text, c.msg = r.Text, api.ClassifyError(r.Kind, r.Text)
	env.Emit(Message, []byte(c.msg))
	return nil
}

// Invariants states the contract as an operator would: a message saying a key
// was rejected is shown only when the error text carries an authentication
// failure from a provider. It reads the text itself and does not call the
// classifier.
func (c *Classifier) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: KeyRejected, Check: func() error {
		if strings.HasSuffix(c.msg, "rejected the key") && !authEvidence(c.text) {
			return fmt.Errorf("%q shown for text with no provider authentication failure", c.msg)
		}
		return nil
	}}}
}

// authEvidence is true when a line of text shows a provider refusing
// credentials. Docker progress such as "403.3kB/40.21MB" is not one.
func authEvidence(text string) bool {
	for _, line := range strings.Split(strings.ToLower(text), "\n") {
		for _, s := range []string{
			"http/1.1 401", "http/1.1 403", "http/2 401", "http/2 403",
			"status code 401", "status code 403", "status_code=401", "status_code=403",
			"error code: 401", "error code: 403", "401 unauthorized", "403 forbidden",
			"no auth credentials", "user not found", "invalid api key", "incorrect api key",
			"token id is invalid", "autherror", "authenticationerror", "unauthorized",
		} {
			if strings.Contains(line, s) {
				return true
			}
		}
	}
	return false
}
