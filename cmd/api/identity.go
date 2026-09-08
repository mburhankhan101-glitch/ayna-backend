package main

import (
	"context"

	iamapp "github.com/ayna/ayna-backend/internal/modules/iam/application"
	iamhttp "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/http"
)

// identityBridge lets skinanalysis ask about a user without importing iam.
//
// This is where the module boundary is actually held. `internal/arch` forbids
// one module's packages from importing another's, and that rule would be
// worthless if the two were wired together by reaching across it. Instead
// skinanalysis declares the narrow port it needs (application.Identity), iam
// exposes its use cases, and the composition root — this file, which is allowed
// to know about both — joins them.
//
// The payoff is concrete rather than architectural piety: when identity becomes
// its own service, this struct grows an HTTP client and nothing else in either
// module changes.
type identityBridge struct {
	current *iamapp.GetCurrentUser
	consent *iamapp.ConsentStatus
}

func (b identityBridge) Resolve(ctx context.Context, subject string) (string, bool, error) {
	u, err := b.current.Execute(ctx, subject)
	if err != nil {
		return "", false, err
	}

	// Consent is version-scoped, so this asks about the wording in force right
	// now. A user who consented to the old policy has not consented to this
	// one, and a scan must not run on stale agreement.
	state, err := b.consent.Execute(ctx, subject, iamhttp.CurrentPolicyVersion)
	if err != nil {
		return "", false, err
	}

	// AllowsScanning rather than a bare boolean: it keeps "never asked" and
	// "asked and declined" distinct, and only the granted case may proceed.
	return u.ID(), state.AllowsScanning(), nil
}
