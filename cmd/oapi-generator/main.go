// Copyright 2020 The go-openapi-tools Authors
// SPDX-License-Identifier: BSD-3-Clause

// Command oapi-generator generates the Go API client code from OpenAPI or Swagger schema.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/zchee/go-openapi-tools/pkg/compiler"
)

const (
	exitSuccess = iota
	exitError
	exitUsage
)

var (
	flagSchemaType  string
	flagPackageName string
	flagOut         string
	flagClean       bool
)

func init() {
	flag.StringVar(&flagSchemaType, "schema", compiler.SchemaNameOpenAPI, fmt.Sprintf("Schema type. one of (%s, %s)", compiler.SchemaNameOpenAPI, compiler.SchemaNameSwagger))
	flag.StringVar(&flagPackageName, "package", "api", "Generate package name.")
	flag.StringVar(&flagOut, "out", ".", "Write schema to specific directory.")
	flag.BoolVar(&flagClean, "clean", false, "clean generated files before generation")
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("oapi-generator: ")

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(exitUsage)
	}
	fname := flag.Arg(0)

	if flagClean {
		if _, err := os.Stat(flagOut); os.IsExist(err) {
			if err := os.Remove(flagOut); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(exitError)
			}
		}
	}

	g, err := compiler.New(flagSchemaType, flagPackageName, fname)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitError)
	}

	if err := g.Generate(flagOut); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitError)
	}
}
