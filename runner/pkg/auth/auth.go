// Package auth decides whether the agent may run an incoming action request.
//
// Two checks, in order:
//
//  1. Relay signature — an Ed25519 signature the relay puts on the raw `body`
//     bytes of every request it forwards. Valid = any action may run.
//  2. Light-action allowlist — read actions that may run without a valid relay
//     signature, so reads keep working on an agent without the relay's public
//     key or under clock skew. Actions in SignedOnly never pass here.
package auth

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/nudgebee/nudgebee-agent/pkg/relaysig"
)

// Request is the in-memory shape of an incoming action request after JSON
// unmarshal. Body is the decoded `body` field; BodyRaw is its exact wire bytes
// (a json.RawMessage, never a re-marshaled map) so it matches the bytes the
// relay signed.
type Request struct {
	Body       map[string]any
	ActionName string
	RelaySig   relaysig.Envelope
	BodyRaw    []byte
}

// SignedRule reports whether one request for an action needs a valid relay
// signature, given its action_params (nil when absent). A nil SignedRule means
// every request for that action does. Rules let an action that both reads and
// writes (kubectl_command_executor, http_proxy_request) keep its reads on the
// light-action path.
type SignedRule func(params map[string]any) bool

// Validator holds the agent's auth state.
//
// LightActions is atomically swappable so refresh_playbook can update the
// allowlist without dropping the WS connection or interleaving with concurrent
// Validate calls. Use SetLightActions for runtime updates.
type Validator struct {
	// LightActions can be set directly at startup; SetLightActions /
	// LightActionsSet hot-swap it under load.
	LightActions map[string]struct{}

	// lightActionsAtomic, when non-nil, takes precedence over LightActions.
	// Set via SetLightActions.
	lightActionsAtomic atomic.Pointer[map[string]struct{}]

	// RelayVerifier validates an Ed25519 relay signature over the raw body.
	// When set and Enabled(), a present+valid relay signature authorizes ANY
	// action. Nil or disabled = the relay signature is ignored and only the
	// light-action allowlist applies.
	RelayVerifier *relaysig.Verifier

	// SignedOnly names actions that never pass on the light-action path, even
	// when the allowlist (startup or refreshed) lists them: they need a valid
	// relay signature. The rule, when not nil, narrows that to the requests it
	// returns true for. Set once at startup; refresh_playbook cannot change it.
	SignedOnly map[string]SignedRule
}

// SetLightActions atomically replaces the light-action allowlist. Concurrent
// Validate calls see either the old or new set, never a torn read. Pass an
// empty map to disable all light actions; pass nil to fall back to the
// startup-set Validator.LightActions.
func (v *Validator) SetLightActions(actions map[string]struct{}) {
	if actions == nil {
		v.lightActionsAtomic.Store(nil)
		return
	}
	// Defensive copy so callers can't mutate the live map under us.
	dup := make(map[string]struct{}, len(actions))
	for k := range actions {
		dup[k] = struct{}{}
	}
	v.lightActionsAtomic.Store(&dup)
}

// LightActionsSet returns the active allowlist (atomic if set, otherwise
// the startup map). Callers must NOT mutate the result.
func (v *Validator) LightActionsSet() map[string]struct{} {
	if p := v.lightActionsAtomic.Load(); p != nil {
		return *p
	}
	return v.LightActions
}

// Validate returns nil if the request may run.
//
// A failed relay signature (bad signature, clock skew, replay, no key) is not
// a hard failure: the request falls through to the light-action allowlist, so
// reads keep working. Falling through never grants extra access: a write in
// SignedOnly or off the allowlist is refused there.
func (v *Validator) Validate(r *Request) error {
	var relayErr error
	switch {
	case v.RelayVerifier == nil || !v.RelayVerifier.Enabled():
		relayErr = errRelayKeyNotConfigured
	case !r.RelaySig.Present():
		relayErr = errNoRelaySignature
	default:
		err := v.RelayVerifier.VerifyBody(r.BodyRaw, r.RelaySig)
		if err == nil {
			return nil // valid relay signature authorizes any action
		}
		// A replay is a request this agent already accepted, delivered again
		// (the broker redelivers what was unacked when a channel dropped). When
		// falling through would reject it, it would be reported as
		// unauthorized while the first delivery may have applied it; say what
		// it is instead. A read falls through and runs as before.
		if errors.Is(err, relaysig.ErrReplay) && !v.isLightRequest(r) {
			return fmt.Errorf("%w: %s", ErrDuplicateDelivery, r.ActionName)
		}
		relayErr = err
	}
	return v.validateLightAction(r, relayErr)
}

var (
	errRelayKeyNotConfigured = errors.New("relay signing key not configured on this agent")
	errNoRelaySignature      = errors.New("no relay signature on the request")
)

// ErrDuplicateDelivery is Validate's error for a mutation whose valid relay
// signature carries a nonce already seen: a second delivery of a request the
// agent accepted before. Its outcome is the first delivery's, not a denial.
var ErrDuplicateDelivery = errors.New("auth: duplicate delivery of a request already received")

// needsSignature reports whether this request may only run with a valid relay
// signature.
func (v *Validator) needsSignature(r *Request) bool {
	rule, ok := v.SignedOnly[r.ActionName]
	if !ok {
		return false
	}
	if rule == nil {
		return true
	}
	params, _ := r.Body["action_params"].(map[string]any)
	return rule(params)
}

// isLightRequest reports whether the request would pass the light-action
// check: on the allowlist and not needing a signature.
func (v *Validator) isLightRequest(r *Request) bool {
	if v.needsSignature(r) {
		return false
	}
	_, ok := v.LightActionsSet()[r.ActionName]
	return ok
}

func (v *Validator) validateLightAction(r *Request, relayErr error) error {
	if v.needsSignature(r) {
		return fmt.Errorf("auth: action %q requires a valid relay signature: %w", r.ActionName, relayErr)
	}
	set := v.LightActionsSet()
	if set == nil {
		return errors.New("auth: light-action mode disabled")
	}
	if _, ok := set[r.ActionName]; !ok {
		return fmt.Errorf("auth: action %q not in light-action allowlist", r.ActionName)
	}
	return nil
}
