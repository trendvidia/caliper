package pxf

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TypeResolver resolves protobuf type URLs to message descriptors.
// Required for encoding/decoding google.protobuf.Any fields with sugar syntax.
type TypeResolver interface {
	FindMessageByURL(url string) (protoreflect.MessageDescriptor, error)
}

// UnmarshalOptions configures PXF decoding.
type UnmarshalOptions struct {
	// TypeResolver resolves type URLs for google.protobuf.Any fields.
	// When set, Any fields use sugar syntax: @type = "..." + inline fields.
	// When nil, Any fields decode as regular messages (type_url + value).
	TypeResolver TypeResolver

	// DiscardUnknown silently skips fields not found in the schema
	// instead of returning an error.
	DiscardUnknown bool
}

// UnmarshalFull decodes PXF data into msg and returns field presence metadata.
// Unlike Unmarshal, it tracks which fields are set, null, or absent,
// validates required fields, and applies default values.
func UnmarshalFull(data []byte, msg proto.Message) (*Result, error) {
	return UnmarshalOptions{}.UnmarshalFull(data, msg)
}

// UnmarshalFull decodes PXF data into msg and returns field presence metadata.
func (o UnmarshalOptions) UnmarshalFull(data []byte, msg proto.Message) (*Result, error) {
	return unmarshalDirectFull(data, msg.ProtoReflect(), o.TypeResolver, o.DiscardUnknown)
}
