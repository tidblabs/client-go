package tikv

import (
	"github.com/tikv/client-go/v2/internal/client"
	tenant_interceptor "github.com/tikv/client-go/v2/keyspace/interceptor"
)

func SetUpTenantInterceptor(interceptor tenant_interceptor.TenantSideKVInterceptor) {
	client.TenantKVControllor = interceptor
}
