package cursor

import (
	"encoding/json"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

func toProtobufValue(v any) (*structpb.Value, error) {
	if v == nil {
		return structpb.NewNullValue(), nil
	}
	return structpb.NewValue(v)
}

func fromProtobufValue(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	raw, err := protojson.Marshal(v)
	if err != nil {
		return v.AsInterface()
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return v.AsInterface()
	}
	// String values are returned verbatim. Re-parsing strings that merely
	// look like JSON corrupts legitimate tool arguments: a shell command
	// "true" became a boolean and a JSON document passed as a write-file
	// content string became an object, which then failed the client tool's
	// schema validation ("no exec result" from the caller's perspective).
	return decoded
}

func decodeMcpArguments(args *agentv1.McpArgs) map[string]any {
	out := map[string]any{}
	if args == nil {
		return out
	}
	for k, v := range args.GetArgs() {
		out[k] = fromProtobufValue(v)
	}
	return out
}
