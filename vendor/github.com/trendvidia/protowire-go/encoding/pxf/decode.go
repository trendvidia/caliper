package pxf

import (
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Unmarshal parses PXF data into msg with default options.
func Unmarshal(data []byte, msg proto.Message) error {
	return UnmarshalOptions{}.Unmarshal(data, msg)
}

// UnmarshalDescriptor parses PXF data using the given message descriptor.
func UnmarshalDescriptor(data []byte, desc protoreflect.MessageDescriptor) (*dynamicpb.Message, error) {
	return UnmarshalOptions{}.UnmarshalDescriptor(data, desc)
}

// UnmarshalFullDescriptor parses PXF data using the given message descriptor
// and returns field presence metadata. It validates required fields and applies defaults.
func UnmarshalFullDescriptor(data []byte, desc protoreflect.MessageDescriptor) (*dynamicpb.Message, *Result, error) {
	return UnmarshalOptions{}.UnmarshalFullDescriptor(data, desc)
}

// Unmarshal parses PXF data into msg.
func (o UnmarshalOptions) Unmarshal(data []byte, msg proto.Message) error {
	return unmarshalDirect(data, msg.ProtoReflect(), o.TypeResolver, o.DiscardUnknown)
}

// UnmarshalDescriptor parses PXF data using the given message descriptor.
func (o UnmarshalOptions) UnmarshalDescriptor(data []byte, desc protoreflect.MessageDescriptor) (*dynamicpb.Message, error) {
	msg := dynamicpb.NewMessage(desc)
	if err := unmarshalDirect(data, msg.ProtoReflect(), o.TypeResolver, o.DiscardUnknown); err != nil {
		return nil, err
	}
	return msg, nil
}

// UnmarshalFullDescriptor parses PXF data using the given message descriptor
// and returns field presence metadata.
func (o UnmarshalOptions) UnmarshalFullDescriptor(data []byte, desc protoreflect.MessageDescriptor) (*dynamicpb.Message, *Result, error) {
	msg := dynamicpb.NewMessage(desc)
	result, err := unmarshalDirectFull(data, msg.ProtoReflect(), o.TypeResolver, o.DiscardUnknown)
	if err != nil {
		return nil, nil, err
	}
	return msg, result, nil
}

func decodeMapKey(fd protoreflect.FieldDescriptor, key string, pos Position) (protoreflect.MapKey, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(key).MapKey(), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(key, 10, 32)
		if err != nil {
			return protoreflect.MapKey{}, errorf(pos, "invalid int32 map key: %s", key)
		}
		return protoreflect.ValueOfInt32(int32(n)).MapKey(), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			return protoreflect.MapKey{}, errorf(pos, "invalid int64 map key: %s", key)
		}
		return protoreflect.ValueOfInt64(n).MapKey(), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(key, 10, 32)
		if err != nil {
			return protoreflect.MapKey{}, errorf(pos, "invalid uint32 map key: %s", key)
		}
		return protoreflect.ValueOfUint32(uint32(n)).MapKey(), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(key, 10, 64)
		if err != nil {
			return protoreflect.MapKey{}, errorf(pos, "invalid uint64 map key: %s", key)
		}
		return protoreflect.ValueOfUint64(n).MapKey(), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(key)
		if err != nil {
			return protoreflect.MapKey{}, errorf(pos, "invalid bool map key: %s", key)
		}
		return protoreflect.ValueOfBool(b).MapKey(), nil
	default:
		return protoreflect.MapKey{}, errorf(pos, "unsupported map key kind: %s", fd.Kind())
	}
}
