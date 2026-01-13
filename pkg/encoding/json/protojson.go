package json

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
)

const Name = "json"

var unmarshaler = protojson.UnmarshalOptions{DiscardUnknown: true}

func init() {
	encoding.RegisterCodec(codec{})
}

type codec struct{}

func (codec) Name() string {
	return Name
}

func (codec) Marshal(v interface{}) ([]byte, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to marshal, message is %T, want proto.Message", v)
	}

	return protojson.Marshal(m)
}

func (codec) Unmarshal(data []byte, v interface{}) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}

	return unmarshaler.Unmarshal(data, m)
}
