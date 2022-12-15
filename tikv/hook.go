package tikv

import (
	"github.com/tikv/client-go/v2/internal/client"
	tenant_interceptor "github.com/tikv/client-go/v2/keyspace/interceptor"
)

func SetUpResourceInterceptor(interceptor tenant_interceptor.ResourceSideKVInterceptor) {
	client.TenantKVControllor = interceptor
}
