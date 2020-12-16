// Copyright 2020 The go-openapi-tools Authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command oapi-generator generates the Go API client code from OpenAPI or Swagger schema.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/zchee/go-openapi-tools/pkg/compiler/oapigen"
)

const (
	exitCode = 1
)

var (
	flagSchemaType  string
	flagPackageName string
	flagOut         string
)

func init() {
	flag.StringVar(&flagSchemaType, "schema", oapigen.OpenAPISchema.String(), "Schema type.")
	flag.StringVar(&flagPackageName, "package", "api", "generate package name.")
	flag.StringVar(&flagOut, "out", "", `write schema to specific directory. (default "current directory")`)
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("oapi-generator: ")

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(exitCode)
	}
	fname := flag.Arg(0)

	api, err := oapigen.NewAPI(fname, flagPackageName, flagSchemaType)
	if err != nil {
		log.Fatal(err)
	}

	if err := api.Gen(flagOut); err != nil {
		log.Fatal(err)
	}
}
