package otlpgrpc

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// validateTopLevelOTLPRequest performs zero-allocation O(Resource) top-level
// protobuf wire framing validation for OTLP Export*ServiceRequest payloads.
// Traces, metrics, and logs export requests all declare field 1 as repeated
// length-delimited Resource* messages (protowire.BytesType); nested fields are
// intentionally not traversed.
func validateTopLevelOTLPRequest(payload []byte) error {
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeField(payload)
		if n < 0 {
			return status.Error(codes.InvalidArgument, "invalid OTLP protobuf wire framing")
		}
		if num == 1 && typ != protowire.BytesType {
			return status.Error(codes.InvalidArgument, "invalid OTLP request field 1 wire type")
		}
		payload = payload[n:]
	}
	return nil
}
