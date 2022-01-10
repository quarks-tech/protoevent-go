package proto

import (
	json "github.com/json-iterator/go"

	"github.com/quarks-tech/protoevent-go/pkg/encoding"
)

const Name = "json"

func init() {
	encoding.RegisterCodec(codec{})
}

type codec struct{}

func (codec) Name() string {
	return Name
}

func (codec) Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (codec) Unmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
