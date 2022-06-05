module txnkv

go 1.16

require github.com/tikv/client-go/v2 v2.0.0

replace github.com/tikv/client-go/v2 => ../../

replace github.com/pingcap/kvproto => github.com/tidblabs/kvproto v0.0.0-20220407200414-24874abbd469

replace github.com/tikv/pd/client => github.com/tidblabs/pd/client v0.0.0-20220412182747-eb76255e4ea5
