package tenantcost

import (
	"github.com/elastic/gosigar"
	"github.com/pingcap/log"
	"go.uber.org/zap"

	"context"
	"os"
)

// GetCPUTime returns the cumulative user/system time (in ms) since the process start.
func GetCPUTime(ctx context.Context) (userTimeMillis, sysTimeMillis int64, err error) {
	pid := os.Getpid()
	cpuTime := gosigar.ProcTime{}
	if err := cpuTime.Get(pid); err != nil {
		return 0, 0, err
	}
	return int64(cpuTime.User), int64(cpuTime.Sys), nil
}

func UserCPUSecs(ctx context.Context) (CPUSecs float64) {
	userTimeMillis, _, err := GetCPUTime(ctx)
	if err != nil {
		log.Error("Failed to get CPU time", zap.Error(err))
		return
	}
	CPUSecs = float64(userTimeMillis) * 1e-3
	return
}
