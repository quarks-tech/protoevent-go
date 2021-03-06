package main

import (
	"flag"
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/pluginpb"
)

const version = "0.1.3"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Printf("protoc-gen-go-pubsub %v\n", version)
		return
	}

	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

		extTypes := new(protoregistry.Types)

		for _, file := range gen.Files {
			if err := registerAllExtensions(extTypes, file.Desc); err != nil {
				panic(err)
			}
		}

		for _, f := range gen.Files {
			if f.Generate {
				if err := generateFile(gen, f, extTypes); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
