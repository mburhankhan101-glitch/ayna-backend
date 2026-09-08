package main

import (
	"context"

	iamhttp "github.com/ayna/ayna-backend/internal/modules/iam/infrastructure/http"
	saapp "github.com/ayna/ayna-backend/internal/modules/skinanalysis/application"
)

// allowanceBridge lets IAM report remaining scans without importing the module
// that counts them.
//
// The same shape as identityBridge, in the other direction: skinanalysis needs
// to know who the caller is, and IAM needs to know what they have left. Both
// are declared as narrow ports by the module that needs them and satisfied
// here, in the one file allowed to know about both. Neither module gains an
// import of the other, so `internal/arch` stays green and either could be
// lifted out to its own service by swapping this struct for an HTTP client.
type allowanceBridge struct{ get *saapp.GetAllowance }

func (b allowanceBridge) For(ctx context.Context, userID string) (iamhttp.Allowance, error) {
	a, err := b.get.Execute(ctx, userID)
	if err != nil {
		return iamhttp.Allowance{}, err
	}
	return iamhttp.Allowance{
		Remaining: a.Remaining,
		Limit:     a.Limit,
		ResetsAt:  a.ResetsAt,
	}, nil
}
