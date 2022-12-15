package client

import (
	"context"
	"time"

	tenant_interceptor "github.com/tikv/client-go/v2/keyspace/interceptor"
	"github.com/tikv/client-go/v2/keyspace/resourcegroup"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/tikvrpc/interceptor"
)

var _ Client = interceptedClient{}

var TenantKVControllor tenant_interceptor.ResourceSideKVInterceptor = nil

type interceptedClient struct {
	Client
}

// NewInterceptedClient creates a Client which can execute interceptor.
func NewInterceptedClient(client Client) Client {
	return interceptedClient{client}
}

func (r interceptedClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	var tenantRPCInterceptor interceptor.RPCInterceptor
	if TenantKVControllor != nil {
		tenantRPCInterceptor = func(next interceptor.RPCInterceptorFunc) interceptor.RPCInterceptorFunc {
			return func(target string, req *tikvrpc.Request) (*tikvrpc.Response, error) {
				reqInfo := resourcegroup.MakeRequestInfo(req)
				err := TenantKVControllor.OnRequestWait(ctx, "demo", reqInfo)
				if err != nil {
					return nil, err
				}
				resp, err := next(target, req)
				if resp != nil {
					respInfo := resourcegroup.MakeResponseInfo(resp)
					TenantKVControllor.OnResponse(context.Background(), "demo", reqInfo, respInfo)
				}
				return resp, err
			}
		}
	}

	var finalInterceptor interceptor.RPCInterceptor
	if tenantRPCInterceptor != nil {
		finalInterceptor = tenantRPCInterceptor
	}
	if it := interceptor.GetRPCInterceptorFromCtx(ctx); it != nil {
		if finalInterceptor != nil {
			finalInterceptor = interceptor.ChainRPCInterceptors(finalInterceptor, it)
		} else {
			finalInterceptor = it
		}
	}
	if finalInterceptor != nil {
		return finalInterceptor(func(target string, req *tikvrpc.Request) (*tikvrpc.Response, error) {
			return r.Client.SendRequest(ctx, target, req, timeout)
		})(addr, req)
	}
	return r.Client.SendRequest(ctx, addr, req, timeout)
}
