package interceptor

import (
	"context"

	resource "github.com/tikv/pd/pkg/mcs/resource_manager/client"
)

type ResourceSideKVInterceptor interface {
	// OnRequestWait accounts for portion of the cost that can be determined
	// upfront. It can block to delay the request as needed, depending on the
	// current allowed rate of resource usage.
	//
	// If the context (or a parent context) was created using
	// WithTenantCostControlExemption, the method is a no-op.
	OnRequestWait(ctx context.Context, resourceGroupName string, info resource.RequestInfo) error

	// OnResponse accounts for the portion of the cost that can only be determined
	// after-the-fact. It does not block, but it can push the rate limiting into
	// "debt", causing future requests to be blocked.
	//
	// If the context (or a parent context) was created using
	// WithTenantCostControlExemption, the method is a no-op.
	OnResponse(ctx context.Context, resourceGroupName string, req resource.RequestInfo, resp resource.ResponseInfo) error
}
