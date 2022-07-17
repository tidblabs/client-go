// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"time"

	coprocessor "github.com/pingcap/kvproto/pkg/coprocessor"
	tenant_interceptor "github.com/tikv/client-go/v2/keyspace/interceptor"
	"github.com/tikv/client-go/v2/keyspace/tenantcost"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/tikvrpc/interceptor"
)

var _ Client = interceptedClient{}

var TenantKVControllor tenant_interceptor.TenantSideKVInterceptor = nil

type interceptedClient struct {
	Client
}

// NewInterceptedClient creates a Client which can execute interceptor.
func NewInterceptedClient(client Client) Client {
	return interceptedClient{client}
}

func FromResponse(resp *tikvrpc.Response) string {
	switch res := resp.Resp.(type) {
	case *coprocessor.Response:
		return res.String()
	}
	return ""
}

func (r interceptedClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	var finalInterceptor interceptor.RPCInterceptor
	if TenantKVControllor != nil {
		finalInterceptor = func(next interceptor.RPCInterceptorFunc) interceptor.RPCInterceptorFunc {
			return func(target string, req *tikvrpc.Request) (*tikvrpc.Response, error) {
				reqInfo := tenantcost.MakeRequestInfo(req)
				err := TenantKVControllor.OnRequestWait(ctx, reqInfo)
				if err != nil {
					return nil, err
				}
				resp, err := next(target, req)
				if resp != nil {
					respInfo := tenantcost.MakeResponseInfo(resp)
					TenantKVControllor.OnResponse(context.Background(), reqInfo, respInfo)
				}
				return resp, err
			}
		}
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
