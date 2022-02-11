// Copyright 2020 The go-openapi-tools Authors.
// SPDX-License-Identifier: BSD-3-Clause

package compiler

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	goformat "go/format"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/jsoninfo"
	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/getkin/kin-openapi/routers"
	json "github.com/goccy/go-json"
	"github.com/klauspost/compress/gzip"
	"github.com/zchee/strcase"
)

// keep related packages on import section.
var (
	_ jsoninfo.StrictStruct
	_ = openapi2conv.ToV3
	_ openapi3filter.ParseErrorKind
	_ openapi3gen.Generator
	_ routers.Route
)

const (
	docFileName    = "doc.go"
	clientFileName = "client.go"
	utilsFileName  = "utils.go"
)

// printFn writes raw or with newline string.
type printFn func(format string, args ...interface{})

// PathItemsMap is the map of PathItems.
//  key:   path
//  value: []*openapi3.PathItem
type PathItemsMap map[string][]*openapi3.PathItem

const (
	SchemaNameSwagger = "swagger"
	SchemaNameOpenAPI = "openapi"
)

// schemaType represents a Schema type. supports OpenAPI or Swagger.
type schemaType uint8

const (
	// unknownSchema is the unkonwn schema.
	unknownSchema schemaType = iota

	// swaggerSchema is the swaggerSchema schema.
	swaggerSchema

	// OpenAPISchema is the OpenAPISchema schema.
	openAPISchema
)

// String returns a string representation of the SchemaType.
func (st schemaType) String() string {
	switch st {
	case swaggerSchema:
		return SchemaNameSwagger
	case openAPISchema:
		return SchemaNameOpenAPI
	default:
		return "unknown"
	}
}

var schemaTypeMap = map[string]schemaType{
	SchemaNameSwagger: swaggerSchema,
	SchemaNameOpenAPI: openAPISchema,
}

func parseSchemaTye(schemaType, filename string) (schemaType, error) {
	schemaType = strings.ToLower(schemaType)
	if st, ok := schemaTypeMap[schemaType]; ok {
		return st, nil
	}

	f, err := os.Open(filename)
	if err != nil {
		return unknownSchema, err
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		switch {
		case strings.Contains(scan.Text(), SchemaNameSwagger):
			return swaggerSchema, nil
		case strings.Contains(scan.Text(), SchemaNameOpenAPI):
			return openAPISchema, nil
		}
	}
	if err := scan.Err(); err != nil {
		return unknownSchema, err
	}

	return unknownSchema, errors.New("unknown schema")
}

type Service struct {
	Name string
	tags openapi3.Tags
}

type Services []*Service

// Generator represents a Go source generator from OpenAPI.
type Generator struct {
	openAPI    *openapi3.T
	schemaType schemaType
	pkgName    string

	buf   *bytes.Buffer
	files map[string][]byte

	services     Services                  // lazy initialize
	servicesOnce sync.Once                 // run GetService once
	methods      map[*Service]PathItemsMap // lazy initialize
	methodsOnce  sync.Once                 // run GetMethods once

	p  printFn // print raw
	pp printFn // print with newline
}

// New parses path JSON file and returns the new Generator.
func New(schemaType, pkgName, filename string) (*Generator, error) {
	st, err := parseSchemaTye(schemaType, filename)
	if err != nil {
		return nil, err
	}
	g := &Generator{
		schemaType: st,
		pkgName:    pkgName,
	}

	// handle path arg
	switch fi, err := os.Stat(filename); {
	case os.IsNotExist(err):
		return nil, fmt.Errorf("not exists %s schema file", filename)

	case fi.IsDir():
		return nil, fmt.Errorf("%s is directory, not schema file", filename)

	case err != nil:
		return nil, err
	}

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	switch g.schemaType {
	case openAPISchema:
		var oai openapi3.T
		if err := dec.Decode(&oai); err != nil {
			return nil, fmt.Errorf("failed to decode %s: %w", filename, err)
		}

		g.openAPI = &oai

	case swaggerSchema:
		var swagger *openapi2.T
		if err := dec.Decode(swagger); err != nil {
			return nil, err
		}

		oai, err := openapi2conv.ToV3(swagger)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %#v to OpenAPI schema: %w", swagger, err)
		}

		g.openAPI = oai
	}

	return g, nil
}

// Generate generates the API from openapi3.Swagger schema.
func (g *Generator) Generate(dst string) (err error) {
	if dst == "" {
		dst, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get cwd: %w", err)
		}
	}

	if err := g.generate(); err != nil {
		return fmt.Errorf("failed to generate: %w", err)
	}

	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("failed to MkdirAll %s: %w", dst, err)
	}

	for filename, data := range g.files {
		f, err := os.Create(filepath.Join(dst, filename))
		if err != nil {
			return fmt.Errorf("failed to Create %s: %w", f.Name(), err)
		}
		if _, err = f.Write(data); err != nil {
			return fmt.Errorf("failed to Write to %s: %w", f.Name(), err)
		}
		// should not use defer
		if err := f.Close(); err != nil {
			return fmt.Errorf("failed to Close to %s: %w", f.Name(), err)
		}
	}

	return nil
}

// generate generates Go source code from OpenAPI spec.
//
// It works sequential, does not needs mutex lock.
func (g *Generator) generate() error {
	g.buf = new(bytes.Buffer)
	g.files = make(map[string][]byte)

	g.p = func(format string, args ...interface{}) {
		_, err := fmt.Fprintf(g.buf, format, args...)
		if err != nil {
			panic(err)
		}
	}
	g.pp = func(format string, args ...interface{}) {
		g.p(format+"\n", args...)
	}

	// write doc.go
	g.WriteHeader()
	g.p("\n")
	g.WriteDoc()
	bufDoc := g.buf.Bytes()
	doc, err := goformat.Source(bufDoc)
	if err != nil {
		log.Println(err)
		doc = bufDoc
	}
	g.files[docFileName] = doc

	// write client.go
	g.buf.Reset()
	g.WriteHeader()
	g.p("\n")
	g.WritePackage()
	g.p("\n")
	g.WriteImports()
	g.p("\n")
	g.WriteConstants()
	g.p("\n")
	g.WriteService()
	g.WriteSchemaDescriptor()

	b := g.buf.Bytes()
	out, err := goformat.Source(b)
	if err != nil {
		log.Println(err)
		out = b
	}
	g.files[clientFileName] = out

	// write api_xxx.go
	for _, tag := range g.GetService() { // []*openapi3.Tag
		fmt.Printf("tag: %#v\n", tag)
		g.buf.Reset()
		g.WriteHeader()
		g.p("\n")
		g.WritePackage()
		g.p("\n")
		g.WriteImports()
		g.p("\n")
		g.WriteAPI(tag)

		bufAPI := g.buf.Bytes()
		api, err := goformat.Source(bufAPI)
		if err != nil {
			log.Printf("api: %s: %#v\n", tag.Name, err)
			api = bufAPI
		}
		g.files[apiFileName(tag.Name)] = api
	}

	// write models
	for name, def := range g.openAPI.Components.Schemas {
		g.buf.Reset()
		g.WriteHeader()
		g.p("\n")
		g.WritePackage()
		g.p("\n")
		g.WriteModel(name, def)

		bufModel := g.buf.Bytes()
		model, err := goformat.Source(bufModel)
		if err != nil {
			log.Printf("model: %s: %#v\n", name, err)
			model = bufModel
		}
		g.files[modelFileName(name)] = model
	}

	// write utils.go
	g.buf.Reset()
	g.WriteHeader()
	g.p("\n")
	g.WritePackage()
	g.p("\n")
	g.WriteImports()
	g.p("\n")

	bufUtils := g.buf.Bytes()
	utils, err := goformat.Source(bufUtils)
	if err != nil {
		log.Printf("utils: %#v\n", err)
		utils = bufUtils
	}
	g.files[utilsFileName] = utils

	return nil
}

func apiFileName(name string) string {
	return "api_" + strcase.ToSnakeCase(name) + ".go"
}

func modelFileName(name string) string {
	return "model_" + strcase.ToSnakeCase(name) + ".go"
}

// GetService gets sorted openapi3.Tags, sorted by openapi3.Tag.Name.
func (g *Generator) GetService() Services {
	g.servicesOnce.Do(func() {
		i := 0
		for methods := range g.GetMethods() {
			if g.services == nil { // lazy initialize
				g.services = make(Services, len(g.methods))
			}
			g.services[i] = methods
			i++
		}

		sort.SliceStable(g.services, func(i, j int) bool { return g.services[i].Name < g.services[j].Name })
	})

	return g.services
}

var defaultService = &Service{Name: "default"}

// GetMethods gets services method from the parses openapi3.Tags.
func (g *Generator) GetMethods() map[*Service]PathItemsMap {
	g.methodsOnce.Do(func() {
		g.methods = make(map[*Service]PathItemsMap)

		// fmt.Printf("a.openAPI.Tags: %#v\n", a.openAPI.Tags)
		switch len(g.openAPI.Tags) {
		case 0:
			g.methods[defaultService] = make(map[string][]*openapi3.PathItem)

		default:
			// initialize a.methods map keys to *openapi3.Tag
			for _, tag := range g.openAPI.Tags {
				s := &Service{
					Name: tag.Name,
					tags: openapi3.Tags{tag},
				}
				g.methods[s] = make(map[string][]*openapi3.PathItem)
			}
		}

		// makes a.methods
		for path, item := range g.openAPI.Paths {
			for _, op := range item.Operations() {
				// fmt.Printf("op.Tags: %T = %#v\n", op.Tags, op.Tags)
				switch len(op.Tags) {
				case 0:
					fmt.Fprintf(os.Stderr, "path: %T = %#v, item: %T = %#v\n", path, path, item, item)
					g.methods[defaultService][path] = append(g.methods[defaultService][path], item)
				case 1:
					// handles multiple tags
					// a.methods map keys are *openapi3.Tag, get actual tag name and compare op.Tags[n]
					for s := range g.methods {
						// append *openapi3.PathItem
						//  s: *openapi3.Tag
						//  path: path
						//  item: *openapi3.PathItem
						g.methods[s][path] = append(g.methods[s][path], item)
					}
				}
			}
		}
	})

	return g.methods
}

const headerFmt = `// Code generated by oapi-generator. DO NOT EDIT.`

// WriteHeader writes license and any file headers.
func (g *Generator) WriteHeader() {
	g.pp(headerFmt)
}

// WriteDoc writes package top level synopsis.
func (g *Generator) WriteDoc() {
	g.pp("// Package %s provides access to the %s REST API.", g.pkgName, Depunct(g.pkgName, true))
}

// WritePackage writes package statement.
func (g *Generator) WritePackage() {
	g.pp("package %s", g.pkgName)
}

type externalPackage struct {
	pkg   string
	alias string
}

// WriteImports writes import section.
func (g *Generator) WriteImports() {
	g.pp("import (")

	// write std packages
	pkgs := []string{
		"bytes",
		"compress/gzip",
		"context",
		"encoding/json",
		"errors",
		"fmt",
		"io",
		"io/ioutil",
		"net/http",
		"net/url",
		"path",
		"strconv",
		"strings",
	}
	for _, pkg := range pkgs {
		g.pp("	%q", pkg)
	}

	g.p("\n")

	// write external packages
	extPkgs := []externalPackage{
		{
			pkg:   "google.golang.org/api/transport/http",
			alias: "htransport",
		},
	}
	for _, ext := range extPkgs {
		g.pp("	%s %q", ext.alias, ext.pkg)
	}
	g.pp(")")

	g.p("\n")

	// write keep imported package pragma
	g.pp("// Always reference these packages, just in case the auto-generated code below doesn't.")
	g.pp("var (")
	g.pp("	_ = bytes.NewBuffer")
	g.pp("	_ = context.Canceled")
	g.pp("	_ = json.NewDecoder")
	g.pp("	_ = errors.New")
	g.pp("	_ = fmt.Sprintf")
	g.pp("	_ = io.Copy")
	g.pp("	_ = ioutil.ReadAll")
	g.pp("	_ = http.NewRequest")
	g.pp("	_ = url.Parse")
	g.pp("	_ = strconv.Itoa")
	g.pp("	_ = path.Join")
	g.pp("	_ = strings.Replace")
	g.pp("	_ = gzip.NewReader")
	g.pp("	_ = htransport.NewClient")
	g.pp(")")
}

// WriteConstants writes constants.
func (g *Generator) WriteConstants() {
	version := g.openAPI.Info.Version

	// exported fields
	g.pp("const (")
	g.pp("	APIVersion = %q", version)
	g.pp("	UserAgent = \"oaigen/\" + APIVersion")
	g.pp(")")

	g.p("\n")

	// unexported fields
	g.pp("const (")
	switch len(g.openAPI.Servers) {
	case 0:
		g.pp("	basePath = %q", "/")
	case 1:
		g.pp("	basePath = %q", g.openAPI.Servers[0].URL)
	}
	g.pp(")")
}

// WriteService writes API Service struct and New function.
func (g *Generator) WriteService() {
	var serviceNames []string // for cache sorted service names

	// write Service struct
	g.pp("// Service represents a %ss.", Depunct(g.pkgName, true)+" Service")
	g.pp("type Service struct {")
	g.pp("	client *http.Client")
	g.pp("	BasePath string // API endpoint base URL")
	g.pp("	UserAgent string // optional additional User-Agent fragment")
	g.p("\n")
	for i, tag := range g.GetService() {
		if serviceNames == nil {
			serviceNames = make([]string, len(g.services)) // lazy initialize
		}
		svcName := Depunct(tag.Name, true)
		g.pp("	%[1]s *%[1]s", svcName)
		serviceNames[i] = svcName
	}
	g.pp("}")

	// write NewService function
	g.pp("// NewService creates a new %s.", Depunct(g.pkgName, true)+" Service")
	g.pp("func NewService(ctx context.Context) (*Service, error) {")
	g.pp("	client, _, err := htransport.NewClient(ctx)")
	g.pp("	if err != nil { return nil, err }\n")
	g.pp("	svc := &Service{client: client, BasePath: basePath}")
	for _, svcName := range serviceNames {
		g.pp("	svc.%[1]s = New%[1]s(svc)", svcName)
	}
	g.p("\n")
	g.pp("	return svc, nil")
	g.pp("}")

	g.p("\n")

	// write userAgent method
	g.pp("func (s *Service) userAgent() string {")
	g.pp("	if s.UserAgent == \"\" { return UserAgent }")
	g.pp("	return UserAgent + \" \" + s.UserAgent")
	g.pp("}")
}

// https://github.com/swagger-api/swagger-codegen/blob/99673744630a/modules/swagger-codegen/src/main/java/io/swagger/codegen/languages/AbstractGoCodegen.java#L62-L80
// https://github.com/OpenAPITools/openapi-generator/blob/19acd36e3af1/modules/openapi-generator/src/main/java/org/openapitools/codegen/languages/AbstractGoCodegen.java#L101-L118
var typeConvMap = map[string]string{
	"integer":    "int32",
	"long":       "int64",
	"number":     "float32",
	"float":      "float32",
	"double":     "float64",
	"BigDecimal": "float64",
	"boolean":    "bool",
	"string":     "string",
	"UUID":       "string",
	"URI":        "string",
	"date":       "string",
	"DateTime":   "time.Time",
	"password":   "string",
	"File":       "*os.File",
	"file":       "*os.File",
	"binary":     "string",
	"ByteArray":  "string",
	"array":      "interface{}",            // TODO(zchee): parse actual type
	"object":     "map[string]interface{}", // TODO(zchee): parse actual type
}

// WriteAPI writes child API service structs and New(Service) function.
func (g *Generator) WriteAPI(tag *Service) {
	svcName := Depunct(tag.Name, true)

	// writes service description, if any
	if tag.tags != nil {
		if description := strings.ToLower(tag.tags.Get(tag.Name).Name); description != "" {
			// add dot if description is not end to dot
			if description[len(description)-1] != '.' {
				description += "."
			}

			g.p("// %s represents ", svcName)
			g.p("a")
			// add 'n' if first letter of description is vowel
			if IsVowel(rune(description[0])) {
				g.p("n")
			}
			g.pp(" %s", description)
		}
	}

	// write service struct
	g.pp("type %s struct {", svcName)
	g.pp("	s *Service")
	g.pp("}")

	// write NewXXX function
	g.pp("// New%[1]s returns the new %[1]s.", svcName)
	g.pp("func New%[1]s(s *Service) *%[1]s {", svcName)
	g.pp("	rs := &%s{s: s}", svcName)
	g.pp("	return rs")
	g.pp("}")

	g.p("\n")

	g.WriteAPIMethods(svcName, tag)
}

const (
	hdrContentType    = "Content-Type"
	hdrAcceptEncoding = "Accept-Encoding"
	mimeJSON          = "application/json"
)

// WriteAPIMethods writes child Service methods.
func (g *Generator) WriteAPIMethods(svcName string, service *Service) {
	operations := make(map[string]map[string]*openapi3.Operation) // map[path]map[method]*openapi3.Operation
	paths := make([]string, 0, len(g.methods[service]))
	methods := make([]string, 0, 7)

	for path, pathItems := range g.methods[service] { // map[path][]*openapi3.PathItem
		paths = append(paths, path)
		for _, item := range pathItems { // []*openapi3.PathItem
			for method, o := range item.Operations() { // map[method]*openapi3.Operation
				methods = append(methods, method)
				o.OperationID = Depunct(o.OperationID, true)
				// Go idiom
				if o.OperationID != "Get" {
					o.OperationID = strings.ReplaceAll(o.OperationID, "Get", "")
				}
				if operations[path] == nil {
					operations[path] = make(map[string]*openapi3.Operation)
				}
				operations[path][method] = o
			}
		}
	}
	sort.Strings(paths)
	sort.Strings(methods)

	seen := make(map[string]bool)
	for _, path := range paths { // []string{paths}
		for _, method := range methods { // []string{paths}
			op, ok := operations[path][method] // map[method]*openapi3.Operation
			if ok {
				methType := fmt.Sprintf("%s%sCall", svcName, op.OperationID)
				if seen[methType] {
					continue
				}

				pm := make(map[string]openapi3.Parameters, 4) // map["path"|"query"|"header"|"cookie"]openapi3.Parameters
				for _, param := range op.Parameters {
					switch param.Value.In {
					case openapi3.ParameterInPath:
						pm[openapi3.ParameterInPath] = append(pm[openapi3.ParameterInPath], param)
					case openapi3.ParameterInQuery:
						pm[openapi3.ParameterInQuery] = append(pm[openapi3.ParameterInQuery], param)
					case openapi3.ParameterInHeader:
						pm[openapi3.ParameterInHeader] = append(pm[openapi3.ParameterInHeader], param)
					case openapi3.ParameterInCookie:
						pm[openapi3.ParameterInCookie] = append(pm[openapi3.ParameterInCookie], param)
					}
				}

				// sort by Parameter.Name
				for _, in := range []string{openapi3.ParameterInPath, openapi3.ParameterInQuery, openapi3.ParameterInHeader, openapi3.ParameterInCookie} {
					sort.SliceStable(pm[in], func(i, j int) bool { return pm[in][i].Value.Name < pm[in][j].Value.Name })
				}

				// sort params by path {xxx} order
				pathParam := make([]*openapi3.ParameterRef, 0, len(pm[openapi3.ParameterInPath]))
				pth := path
				for {
					idx := strings.Index(pth, "{")
					if idx == -1 {
						break
					}
					endIdx := strings.Index(pth[idx+1:], "}")
					for _, param := range pm[openapi3.ParameterInPath] {
						if pth[idx+1:idx+1+endIdx] == param.Value.Name {
							pathParam = append(pathParam, param)
						}
					}
					pth = pth[idx+endIdx:]
				}

				// writes operation summary, if any
				if summary := strings.ToLower(op.Summary); summary != "" {
					// add dot if summary is not end to dot
					if summary[len(summary)-1] != '.' {
						summary += "."
					}

					g.pp("// %s provides the %s", methType, summary)
				}

				// write service struct
				g.pp("type %s struct {", methType)
				g.pp("	s *Service")
				g.pp("	header http.Header")
				g.pp("	params url.Values")
				g.p("\n")

				// write path fields
				if len(pathParam) > 0 {
					g.pp("	// path fields")
					for _, param := range pathParam { // []*openapi3.ParameterRef
						paramName := NormalizeParam(Depunct(param.Value.Name, false))
						paramType, ok := typeConvMap[param.Value.Schema.Value.Type]
						if !ok {
							continue
						}

						g.pp("	%s %s", paramName, paramType)
					}
				}
				// write query fields
				if len(pm[openapi3.ParameterInQuery]) > 0 {
					g.pp("	// query fields")
					for _, param := range pm[openapi3.ParameterInQuery] { // []*openapi3.ParameterInQuery
						paramName := NormalizeParam(Depunct(param.Value.Name, false))
						if param.Value.Schema == nil {
							continue
						}
						paramType, ok := typeConvMap[param.Value.Schema.Value.Type]
						if !ok {
							continue
						}

						g.pp("	%s %s", paramName, paramType)
					}
				}
				g.pp("}")
				seen[methType] = true

				g.p("\n")

				// writes operation summary, if any
				if summary := strings.ToLower(op.Summary); summary != "" {
					// add dot if summary is not end to dot
					if summary[len(summary)-1] != '.' {
						summary += "."
					}

					g.pp("// %s returns the %s for %s", op.OperationID, methType, summary)
				}

				// write method
				g.p("func (r *%s) %s(", svcName, op.OperationID)
				if len(pathParam) > 0 {
					for i, param := range pathParam {
						g.p("%s %s", Depunct(param.Value.Name, false), param.Value.Schema.Value.Type)
						if i < len(pathParam)-1 {
							g.p(", ")
						}
					}
				}
				g.pp(") *%s {", methType)
				g.pp("	c := &%s{", methType)
				g.pp("		s: r.s,")
				g.pp("		header: make(http.Header),")
				g.pp("		params: url.Values{},")
				if len(pathParam) > 0 {
					for _, param := range pathParam {
						g.pp("		%[1]s: %[1]s,", Depunct(param.Value.Name, false))
					}
				}
				g.pp("	}")
				g.pp("	return c")
				g.pp("}")

				g.p("\n")

				// write query method chains
				for _, param := range pm[openapi3.ParameterInQuery] { // []*openapi3.Parameter
					paramName := NormalizeParam(Depunct(param.Value.Name, false))
					argName := Depunct(paramName, true)
					typeName := paramName
					if param.Value.Schema == nil {
						continue
					}
					g.pp("func (c *%[1]s) %[2]s(%[3]s %[4]s) *%[1]s {", methType, argName, typeName, typeConvMap[param.Value.Schema.Value.Type])
					g.pp("	c.params.Set(%[1]q, fmt.Sprintf(\"%%v\", %[1]s))", typeName)
					g.pp("	return c")
					g.pp("}")
					g.p("\n")
				}

				g.p("\n")

				// replace {xxx} in path
				if len(pathParam) > 0 {
					for _, param := range pathParam {
						idx := strings.Index(path, "{")
						if idx == -1 {
							break
						}
						endIdx := strings.Index(path[idx+1:], "}")

						path = path[:idx] + `" + ` + "fmt.Sprintf(\"%v\", c." + Depunct(param.Value.Name, false) + ")" + ` + "` + path[idx+1+endIdx+1:]
					}
				}
				methodType := "http.Method" + Depunct(method, true)

				// write request
				g.pp("// Do executes the %s.", svcName+op.OperationID)
				g.pp("func (c *%s) Do(ctx context.Context) (interface{}, error) {", methType)
				g.pp("	uri := path.Join(c.s.BasePath, \"%s\")", path)
				g.pp("	if len(c.params) > 0 {")
				g.pp("		uri += \"?\" + c.params.Encode()")
				g.pp("	}")
				g.p("\n")
				g.pp("	req, err := http.NewRequest(%s, uri, nil)", methodType)
				g.pp("	if err != nil { return nil, err }")
				g.p("\n")
				g.pp("	req.Header.Set(%q, %q)", hdrContentType, mimeJSON)
				g.pp("	req.Header.Set(%q, %q)", hdrAcceptEncoding, mimeJSON)
				g.p("\n")
				g.pp("	resp, err := c.s.client.Do(req.WithContext(ctx))")
				g.pp("	if err != nil { return nil, err }")
				g.pp("	defer resp.Body.Close()")
				g.p("\n")
				g.pp("	if resp.StatusCode != 200 {")
				g.pp("		return nil, errors.New(resp.Status)")
				g.pp("	}")
				g.p("\n")
				g.pp("	body, err := ioutil.ReadAll(resp.Body)")
				g.pp("	if err != nil { return nil, err }")
				g.p("\n")
				g.pp("	var result interface{}") // TODO(zchee): actual Response type
				g.pp("	if err := json.Unmarshal(body, &result); err != nil {")
				g.pp("		return nil, err")
				g.pp("	}")
				g.p("\n")
				g.pp("	return result, nil")
				g.pp("}\n")
			}
		}
	}
}

// WriteModel writes model definitions.
func (g *Generator) WriteModel(name string, component *openapi3.SchemaRef) {
	if component.Value != nil && component.Value.Properties != nil {
		// sort fields
		fields := make([]string, len(component.Value.Properties))
		i := 0
		for field := range component.Value.Properties {
			fields[i] = field
			i++
		}
		sort.Strings(fields)

		g.p("// %s represents ", Depunct(name, true))
		desc := fmt.Sprintf("a model of %s.", Depunct(name, false))
		if description := strings.ToLower(component.Value.Description); description != "" {
			// add dot if description is not end to dot
			if description[len(description)-1] != '.' {
				description += "."
			}

			desc = "a"
			// add 'n' if first letter of description is vowel
			if IsVowel(rune(description[0])) {
				desc += "n"
			}
			desc += " " + description
		}
		g.pp(desc)

		fieldTypes := make(map[string]string)
		g.pp("type %s struct {", Depunct(name, true))
		for _, field := range fields {
			property := component.Value.Properties[field]
			// TODO(zchee): parse actual field type
			if property.Value != nil {
				switch val := property.Value; val.Type {
				case "object":
					if val.AdditionalProperties != nil && val.AdditionalProperties.Value != nil {
						switch objVal := val.AdditionalProperties.Value; objVal.Type {
						case "array":
							if objVal.Items.Value != nil {
								typ, ok := typeConvMap[objVal.Items.Value.Type]
								if ok {
									g.pp("	%s []%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
									fieldTypes[field] = "[]" + typ
								}
							}

						case "object":
							t := objVal.Type
							if objVal.Items != nil {
								t = objVal.Items.Value.Type
							}
							typ, ok := typeConvMap[t]
							if ok {
								g.pp("	%s %s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
								fieldTypes[field] = typ
							}
						default:
							typ, ok := typeConvMap[objVal.Type]
							if ok {
								g.pp("	%s *%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
								fieldTypes[field] = typ
							}
						}
					} else {
						typ, ok := typeConvMap[val.Type]
						if ok {
							g.pp("	%s %s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
							fieldTypes[field] = typ
						}
					}
				case "array":
					if val.Items.Value != nil {
						typ, ok := typeConvMap[val.Items.Value.Type]
						if ok {
							g.pp("	%s []%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
							fieldTypes[field] = "[]" + typ
						}
					}

				default:
					typ, ok := typeConvMap[val.Type]
					if ok {
						g.pp("	%s *%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
						fieldTypes[field] = typ
					}
				}
			}
		}
		g.pp("}")

		for _, field := range fields {
			reciever := strings.ToLower(string(name[0]))
			fieldType := fieldTypes[field]
			if fieldType == "" {
				continue
			}
			field := Depunct(field, true)

			g.pp("// Get%[1]s returns the %[1]s field value if set, zero value otherwise.", field)
			g.pp("func (%s *%s) Get%s() (ret %s) {", reciever, Depunct(name, true), field, fieldType)
			g.pp(" 	if %[1]s == nil || %[1]s.%[2]s == nil {", reciever, field)
			g.pp(" 		return ret")
			g.pp(" 	}")
			// TODO(zchee): parse actual field type
			switch fieldType {
			case "map[string]interface{}", "interface{}":
				g.pp(" 	return %s.%s", reciever, field)
			default:
				g.p(" 	return ")
				if !strings.HasPrefix(fieldType, "[]") {
					g.p("*")
				}
				g.pp("%s.%s", reciever, field)
			}
			g.pp("}")

			g.p("\n")

			g.pp("// Has%s reports whether the field has been set", field)
			g.pp("func (%s *%s) Has%s() bool {", reciever, Depunct(name, true), field)
			g.pp(" 	if %[1]s != nil && %[1]s.%[2]s != nil {", reciever, field)
			g.pp(" 		return true")
			g.pp(" 	}")
			g.pp(" 	return false")
			g.pp("}")

			g.p("\n")

			g.pp("// Set%[1]s gets a reference to the given string and assigns it to the %[1]s field.", field)

			// TODO(zchee): parse actual field type
			switch {
			case fieldType == "map[string]interface{}", fieldType == "interface{}":
				g.pp("func (%s *%s) Set%s(val %s) {", reciever, Depunct(name, true), field, fieldType)
				g.pp("	%s.%s = val", reciever, field)

			default:
				if typ, ok := typeConvMap[fieldType]; ok {
					var hasPtr bool
					if !strings.HasPrefix(typ, "[]") {
						hasPtr = true
					}
					g.p("func (%s ", reciever)
					if hasPtr {
						g.p("*")
					}
					g.pp("%s) Set%s(val %s) {", Depunct(name, true), field, typ)

					g.p("	%s.%s = ", reciever, field)
					if hasPtr {
						g.p("&")
					}
					g.pp("val")
				}
			}
			g.pp("}")
		}
	}
}

// WriteSchemaDescriptor writes base64 encoded, gzipped compressed and JSON marshaled schema spec into generated file.
func (g *Generator) WriteSchemaDescriptor() {
	if g.openAPI == nil {
		return // no-op
	}

	in := g.openAPI
	data, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}

	buf := &bytes.Buffer{}
	zw, err := gzip.NewWriterLevel(buf, gzip.BestCompression)
	if err != nil {
		panic(err)
	}
	if _, err := zw.Write(data); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	b := buf.Bytes()

	g.pp("// SchemaDescriptor returns the Schema file descriptor which is generated code to this file.")
	g.pp("func SchemaDescriptor() (interface{}, error) {")
	g.pp("	zr, err := gzip.NewReader(bytes.NewReader(fileDescriptor))")
	g.pp("	if err != nil { return nil, err }")
	g.p("\n")
	g.pp("	var buf bytes.Buffer")
	g.pp("	_, err = buf.ReadFrom(zr)")
	g.pp("	if err != nil { return nil, err }")
	g.p("\n")
	g.pp("	var v interface{}")
	g.pp("	if err := json.NewDecoder(bytes.NewReader(buf.Bytes())).Decode(&v); err != nil {")
	g.pp("		return nil, err")
	g.pp("	}")
	g.p("\n")
	g.pp("	return v, nil")
	g.pp("}")

	g.p("\n")

	g.pp("// fileDescriptor gzipped JSON marshaled Schema object.")
	g.pp("var fileDescriptor = []byte{")
	g.pp("	// %d bytes of a gzipped Schema file descriptor", len(b))

	for len(b) > 0 {
		n := 16
		if n > len(b) {
			n = len(b)
		}

		s := ""
		for _, c := range b[:n] {
			s += fmt.Sprintf("0x%02x, ", c)
		}
		g.pp("	%s", s)

		b = b[n:]
	}
	g.pp("}")
}
