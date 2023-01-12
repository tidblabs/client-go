package resourcegroup

import (
	"unsafe"

	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/tikv/client-go/v2/tikvrpc"
)

type RequestInfo struct {

	// writeBytes is the write size if the request is a write, or -1 if it is a read.
	writeBytes int64
}

// MakeRequestInfo extracts the relevant information from a BatchRequest.
func MakeRequestInfo(req *tikvrpc.Request) *RequestInfo {
	if !req.IsTxnWriteRequest() && !req.IsRawWriteRequest() {
		return &RequestInfo{writeBytes: -1}
	}

	var writeBytes int64
	switch r := req.Req.(type) {
	case *kvrpcpb.PrewriteRequest:
		writeBytes += int64(r.TxnSize)
	case *kvrpcpb.CommitRequest:
		writeBytes += int64(unsafe.Sizeof(r.Keys))
	}

	return &RequestInfo{writeBytes: writeBytes}
}

// IsWrite returns whether the request is a write, and if so the write size in
// bytes.
func (req *RequestInfo) IsWrite() bool {
	return req.writeBytes > -1
}

func (req *RequestInfo) WriteBytes() uint64 {
	return uint64(req.writeBytes)
}

type ResponseInfo struct {
	readBytes int64
}

// MakeResponseInfo extracts the relevant information from a BatchResponse.
func MakeResponseInfo(resp *tikvrpc.Response) *ResponseInfo {
	var (
		readBytes int64
		detailV2  *kvrpcpb.ExecDetailsV2
		detail    *kvrpcpb.ExecDetails
	)
	if resp.Resp == nil {
		return &ResponseInfo{readBytes}
	}
	switch r := resp.Resp.(type) {
	case *coprocessor.Response:
		detailV2 = r.ExecDetailsV2
		detail = r.ExecDetails
	case *tikvrpc.CopStreamResponse:
		// streaming request returns io.EOF, so the first CopStreamResponse.Response maybe nil.
		if r.Response != nil {
			detailV2 = r.Response.ExecDetailsV2
			detail = r.Response.ExecDetails
		}
	case *kvrpcpb.GetResponse:

	default:
		// log.Warn("[kv resource]unreachable resp type", zap.Any("type", reflect.TypeOf(r)))
		return &ResponseInfo{readBytes}
	}

	if detailV2 != nil && detailV2.TimeDetail != nil {
		readBytes = int64(detailV2.ScanDetailV2.RocksdbBlockReadByte)
	} else if detail != nil && detail.TimeDetail != nil {
		readBytes = detail.ScanDetail.Lock.ReadBytes + detail.ScanDetail.Write.ReadBytes + detail.ScanDetail.Write.ReadBytes
	}

	return &ResponseInfo{readBytes: readBytes}
}

func (res *ResponseInfo) ReadBytes() uint64 {
	return uint64(res.readBytes)
}

func (res *ResponseInfo) KVCPUms() uint64 {
	return 10
}
