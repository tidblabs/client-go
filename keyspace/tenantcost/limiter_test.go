// Package rate provides a rate limiter.
package tenantcost

// import (
// 	"context"
// 	"testing"
// 	"time"

// 	"github.com/stretchr/testify/suite"
// )

// func TestLimiter(t *testing.T) {
// 	suite.Run(t, new(testLimiterSuite))
// }

// type testLimiterSuite struct {
// 	suite.Suite
// }

// func (s *testLimiterSuite) TestLimiterWaintN() {
// 	c := make(chan struct{})
// 	limiter := NewLimiter(initialRate, initialRquestUnits, c)
// 	err := limiter.WaitN(context.Background(), 1)
// 	s.Nil(err)
// 	s.Equal(initialRquestUnits-1, int(limiter.AvailableTokens(time.Now())))
// }
