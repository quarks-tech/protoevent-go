package proto

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
)

const Name = "proto"

func init() {
	encoding.RegisterCodec(codec{})
}

type codec struct{}

func (codec) Name() string {
	return Name
}

func (codec) Marshal(v interface{}) ([]byte, error) {
	vv, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to marshal, message is %T, want proto.Message", v)
	}
	b, err := proto.Marshal(vv)
	if err != nil {
		return nil, fmt.Errorf("proto codec: marshal: %w", err)
	}
	return b, nil
}

func (codec) Unmarshal(data []byte, v interface{}) error {
	vv, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}
	if err := proto.Unmarshal(data, vv); err != nil {
		return fmt.Errorf("proto codec: unmarshal: %w", err)
	}
	return nil
}
