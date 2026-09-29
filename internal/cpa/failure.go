package cpa

import "github.com/Newoahil/CPA-Manager/internal/domain"

// Only OUTER management status is classified here. Upstream status is passed
// separately to quota.Parse. Response text is never promoted to quota evidence.
func classifyStatus(status int) domain.FailureKind {
	switch {
	case status == 401 || status == 403:
		return domain.FailureControlPlane
	case status == 404 || status == 501:
		return domain.FailureUnsupported
	case status == 408 || status == 429 || status >= 500:
		return domain.FailureTransport
	default:
		return domain.FailureParse
	}
}
