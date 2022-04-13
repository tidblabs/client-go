module txnkv

go 1.18

require github.com/tikv/client-go/v2 v2.0.0-00010101000000-000000000000

replace github.com/tikv/client-go/v2 => ../../

replace github.com/pingcap/kvproto => github.com/tidblabs/kvproto v0.0.0-20220717141846-8f5445390a32

replace github.com/tikv/pd/client => github.com/tidblabs/pd/client v0.0.0-20220717143221-433427468de1


