// Copyright 2020 The go-openapi-tools Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package oapigen

import (
	"bytes"
	"fmt"
	goformat "go/format"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/jsoninfo"
	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/openapi3gen"
	"github.com/getkin/kin-openapi/pathpattern"
	jsoniter "github.com/json-iterator/go"
	"github.com/klauspost/compress/gzip"
	"github.com/zchee/strcase"
)

// keep related packages on import section.
var (
	_ jsoninfo.StrictStruct
	_ = openapi2conv.ToV3Swagger
	_ openapi3filter.ParseErrorKind
	_ openapi3gen.Generator
	_ pathpattern.Node
)

const (
	docFileName    = "doc.go"
	clientFileName = "client.go"
	utilsFileName  = "utils.go"
)

// PrintFn writes raw or with newline string.
type PrintFn func(format string, args ...interface{})

// PathItemsMap is the map of PathItems.
//  key:   path
//  value: []*openapi3.PathItem
type PathItemsMap map[string][]*openapi3.PathItem

// SchemaType represents a Schema type. supports OpenAPI or Swagger.
type SchemaType uint8

const (
	// UnkonwnSchema is the unkonwn schema.
	UnkonwnSchema SchemaType = iota

	// OpenAPISchema is the OpenAPISchema schema.
	OpenAPISchema

	// SwaggerSchema is the SwaggerSchema schema.
	SwaggerSchema
)

// String returns a string representation of the SchemaType.
func (st SchemaType) String() string {
	switch st {
	case OpenAPISchema:
		return "OpenAPI"
	case SwaggerSchema:
		return "Swagger"
	default:
		return "Unkonwn"
	}
}

// SchemaTypeFromString parses s string and returns the correspond SchemaType.
func SchemaTypeFromString(s string) SchemaType {
	switch strings.ToLower(s) {
	case strings.ToLower(OpenAPISchema.String()):
		return OpenAPISchema
	case strings.ToLower(SwaggerSchema.String()):
		return SwaggerSchema
	}

	return UnkonwnSchema
}

type Service struct {
	Name string
	tags openapi3.Tags
}

type Services []*Service

// API represents an Spinnaker API to generate, as well as its state while it's
// generating.
type API struct {
	openAPI *openapi3.Swagger
	typ     SchemaType
	pkgName string

	buf   *bytes.Buffer
	files map[string][]byte // map[filename][]byte{data}

	services     Services                  // underlyng type: []*openapi3.Tag
	servicesOnce sync.Once                 // run GetService once
	methods      map[*Service]PathItemsMap // map[*Services]map[path][]*openapi3.PathItem
	methodsOnce  sync.Once                 // run GetMethods once

	p  PrintFn // print raw
	pp PrintFn // print with newline
}

// NewAPI parses path JSON file and returns the new API.
func NewAPI(path, name string, schemaType string) (*API, error) {
	api := &API{
		typ:     SchemaTypeFromString(schemaType),
		pkgName: name,
	}

	// handle path arg
	switch fi, err := os.Stat(path); {
	case os.IsNotExist(err):
		return nil, fmt.Errorf("not exists %s", path)
	case fi.IsDir():
		return nil, fmt.Errorf("%s is directory, not schema file", path)
	case err != nil:
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := jsoniter.ConfigFastest.NewDecoder(f)
	switch api.typ {
	case OpenAPISchema:
		var oai openapi3.Swagger
		if err := dec.Decode(&oai); err != nil {
			return nil, fmt.Errorf("failed to decode %s: %w", path, err)
		}
		api.openAPI = &oai

	case SwaggerSchema:
		var swagger *openapi2.Swagger
		if err := dec.Decode(swagger); err != nil {
			return nil, err
		}
		to, err := openapi2conv.ToV3Swagger(swagger)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %#v to OpenAPI schema: %w", swagger, err)
		}
		api.openAPI = to
	}

	return api, nil
}

// Gen generates the API from openapi3.Swagger schema.
func (a *API) Gen(dst string) error {
	if dst == "" {
		dst, _ = os.Getwd()
	}

	if err := a.gen(); err != nil {
		return fmt.Errorf("failed to generate: %w", err)
	}

	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("failed to MkdirAll %s: %w", dst, err)
	}

	for filename, data := range a.files {
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

// gen generates Go source code from OpenAPI spec. It works sequential, does not needs mutex lock.
func (a *API) gen() error {
	a.buf = &bytes.Buffer{}
	a.files = make(map[string][]byte)

	a.p = func(format string, args ...interface{}) {
		_, err := fmt.Fprintf(a.buf, format, args...)
		if err != nil {
			panic(err)
		}
	}
	a.pp = func(format string, args ...interface{}) {
		a.p(format+"\n", args...)
	}

	p, pp := a.p, a.pp

	// write doc.go
	a.WriteHeader(p, pp)
	p("\n")
	a.WriteDoc(p, pp)
	bufDoc := a.buf.Bytes()
	doc, err := goformat.Source(bufDoc)
	if err != nil {
		log.Println(err)
		doc = bufDoc
	}
	a.files[docFileName] = doc

	// write client.go
	a.buf.Reset()
	a.WriteHeader(p, pp)
	p("\n")
	a.WritePackage(p, pp)
	p("\n")
	a.WriteImports(p, pp)
	p("\n")
	a.WriteConstants(p, pp)
	p("\n")
	a.WriteService(p, pp)
	a.WriteSchemaDescriptor(p, pp)
	b := a.buf.Bytes()
	out, err := goformat.Source(b)
	if err != nil {
		log.Println(err)
		out = b
	}
	a.files[clientFileName] = out

	// write api_xxx.go
	for _, tag := range a.GetService() { // []*openapi3.Tag
		a.buf.Reset()
		a.WriteHeader(p, pp)
		p("\n")
		a.WritePackage(p, pp)
		p("\n")
		a.WriteImports(p, pp)
		p("\n")
		a.WriteAPI(p, pp, tag)
		bufAPI := a.buf.Bytes()
		api, err := goformat.Source(bufAPI)
		if err != nil {
			log.Printf("api: %s: %#v\n", tag.Name, err)
			api = bufAPI
		}
		a.files[apiFileName(tag.Name)] = api
	}

	// write models
	for name, def := range a.openAPI.Components.Schemas {
		a.buf.Reset()
		a.WriteHeader(p, pp)
		p("\n")
		a.WritePackage(p, pp)
		p("\n")
		a.WriteModel(p, pp, name, def)
		bufModel := a.buf.Bytes()
		model, err := goformat.Source(bufModel)
		if err != nil {
			log.Printf("model: %s: %#v\n", name, err)
			model = bufModel
		}
		a.files[modelFileName(name)] = model
	}

	// write utils.go
	a.buf.Reset()
	a.WriteHeader(p, pp)
	p("\n")
	a.WritePackage(p, pp)
	p("\n")
	a.WriteImports(p, pp)
	p("\n")
	bufUtils := a.buf.Bytes()
	utils, err := goformat.Source(bufUtils)
	if err != nil {
		log.Printf("utils: %#v\n", err)
		utils = bufUtils
	}
	a.files[utilsFileName] = utils

	return nil
}

func apiFileName(name string) string {
	return "api_" + strcase.ToSnakeCase(name) + ".go"
}

func modelFileName(name string) string {
	return "model_" + strcase.ToSnakeCase(name) + ".go"
}

// GetService gets sorted openapi3.Tags, sorted by openapi3.Tag.Name.
func (a *API) GetService() Services {
	a.servicesOnce.Do(func() {
		i := 0
		for methods, _ := range a.GetMethods() {
			if a.services == nil { // lazy initialize
				a.services = make(Services, len(a.methods))
			}
			a.services[i] = methods
			i++
		}
		sort.SliceStable(a.services, func(i, j int) bool { return a.services[i].Name < a.services[j].Name })
	})

	return a.services
}

var defaultService = &Service{Name: "default"}

// GetMethods gets services method from the parses openapi3.Tags.
func (a *API) GetMethods() map[*Service]PathItemsMap {
	a.methodsOnce.Do(func() {
		a.methods = make(map[*Service]PathItemsMap)

		switch len(a.openAPI.Tags) {
		case 0:
			a.methods[defaultService] = make(map[string][]*openapi3.PathItem)
		case 1:
			// initialize a.methods map keys to *openapi3.Tag
			for _, tag := range a.openAPI.Tags {
				if tag.Name == "" {
					continue
				}
				a.methods[&Service{tags: openapi3.Tags{tag}}] = make(map[string][]*openapi3.PathItem)
			}
		}

		// makes a.methods
		for path, item := range a.openAPI.Paths {
			for _, op := range item.Operations() {
				fmt.Printf("op.Tags: %T = %#v\n", op.Tags, op.Tags)
				switch len(op.Tags) {
				case 0:
					fmt.Fprintf(os.Stderr, "path: %T = %#v, item: %T = %#v\n", path, path, item, item)
					a.methods[defaultService][path] = append(a.methods[defaultService][path], item)
				case 1:
					// handles multiple tags
					// for _, tag := range op.Tags {
					// a.methods map keys are *openapi3.Tag, get actual tag name and compare op.Tags[n]
					for s := range a.methods {
						// if s.Name == tag {
						// append *openapi3.PathItem
						//  s: *openapi3.Tag
						//  path: path
						//  item: *openapi3.PathItem
						a.methods[s][path] = append(a.methods[s][path], item)
						// }
					}
					// }
				}
			}
		}
	})

	return a.methods
}

const headerFmt = `// Copyright %d All rights reserved.
	
// Code generated file. DO NOT EDIT.`

// WriteHeader writes license and any file headers.
func (a *API) WriteHeader(p, pp PrintFn) {
	pp(headerFmt, time.Now().Year())
}

// WriteDoc writes package top level synopsis.
func (a *API) WriteDoc(p, pp PrintFn) {
	pp("// Package %s provides access to the %s REST API.", a.pkgName, Depunct(a.pkgName, true))
}

// WritePackage writes package statement.
func (a *API) WritePackage(p, pp PrintFn) {
	pp("package %s", a.pkgName)
}

type externalPackage struct {
	pkg   string
	alias string
}

// WriteImports writes import section.
func (a *API) WriteImports(p, pp PrintFn) {
	pp("import (")

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
		pp("	%q", pkg)
	}

	p("\n")

	// write external packages
	extPkgs := []externalPackage{
		{
			pkg:   "google.golang.org/api/transport/http",
			alias: "htransport",
		},
	}
	for _, ext := range extPkgs {
		pp("	%s %q", ext.alias, ext.pkg)
	}
	pp(")")

	p("\n")

	// write keep imported package pragma
	pp("// Always reference these packages, just in case the auto-generated code below doesn't.")
	pp("var (")
	pp("	_ = bytes.NewBuffer")
	pp("	_ = context.Canceled")
	pp("	_ = json.NewDecoder")
	pp("	_ = errors.New")
	pp("	_ = fmt.Sprintf")
	pp("	_ = io.Copy")
	pp("	_ = ioutil.ReadAll")
	pp("	_ = http.NewRequest")
	pp("	_ = url.Parse")
	pp("	_ = strconv.Itoa")
	pp("	_ = path.Join")
	pp("	_ = strings.Replace")
	pp("	_ = gzip.NewReader")
	pp("	_ = htransport.NewClient")
	pp(")")
}

// WriteConstants writes constants.
func (a *API) WriteConstants(p, pp PrintFn) {
	version := a.openAPI.Info.Version

	// exported fields
	pp("const (")
	pp("	APIVersion = %q", version)
	pp("	UserAgent = \"oaigen/\" + APIVersion")
	pp(")")

	p("\n")

	// unexported fields
	pp("const (")
	switch len(a.openAPI.Servers) {
	case 0:
		pp("	basePath = %q", "/")
	case 1:
		pp("	basePath = %q", a.openAPI.Servers[0].URL)
	}
	pp(")")
}

// WriteService writes API Service struct and New function.
func (a *API) WriteService(p, pp PrintFn) {
	var serviceNames []string // for cache sorted service names

	// write Service struct
	pp("// Service represents a %ss.", Depunct(a.pkgName, true)+" Service")
	pp("type Service struct {")
	pp("	client *http.Client")
	pp("	BasePath string // API endpoint base URL")
	pp("	UserAgent string // optional additional User-Agent fragment")
	p("\n")
	for i, tag := range a.GetService() {
		if serviceNames == nil {
			serviceNames = make([]string, len(a.services)) // lazy initialize
		}
		svcName := Depunct(tag.Name, true)
		pp("	%[1]s *%[1]s", svcName)
		serviceNames[i] = svcName
	}
	pp("}")

	// write NewService function
	pp("// NewService creates a new %s.", Depunct(a.pkgName, true)+" Service")
	pp("func NewService(ctx context.Context) (*Service, error) {")
	pp("	client, _, err := htransport.NewClient(ctx)")
	pp("	if err != nil { return nil, err }\n")
	pp("	svc := &Service{client: client, BasePath: basePath}")
	for _, svcName := range serviceNames {
		pp("	svc.%[1]s = New%[1]s(svc)", svcName)
	}
	p("\n")
	pp("	return svc, nil")
	pp("}")

	p("\n")

	// write userAgent method
	pp("func (s *Service) userAgent() string {")
	pp("	if s.UserAgent == \"\" { return UserAgent }")
	pp("	return UserAgent + \" \" + s.UserAgent")
	pp("}")
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
func (a *API) WriteAPI(p, pp PrintFn, tag *Service) {
	svcName := Depunct(tag.Name, true)

	// writes service description, if any
	if tag.tags != nil {
		if description := strings.ToLower(tag.tags.Get(tag.Name).Name); description != "" {
			// add dot if description is not end to dot
			if description[len(description)-1] != '.' {
				description += "."
			}

			p("// %s represents ", svcName)
			p("a")
			// add 'n' if first letter of description is vowel
			if IsVowel(rune(description[0])) {
				p("n")
			}
			pp(" %s", description)
		}
	}

	// write service struct
	pp("type %s struct {", svcName)
	pp("	s *Service")
	pp("}")

	// write NewXXX function
	pp("// New%[1]s returns the new %[1]s.", svcName)
	pp("func New%[1]s(s *Service) *%[1]s {", svcName)
	pp("	rs := &%s{s: s}", svcName)
	pp("	return rs")
	pp("}")

	p("\n")

	a.WriteAPIMethods(p, pp, svcName, tag)
}

const (
	hdrContentType    = "Content-Type"
	hdrAcceptEncoding = "Accept-Encoding"
	mimeJSON          = "application/json"
)

// WriteAPIMethods writes child Service methods.
func (a *API) WriteAPIMethods(p, pp PrintFn, svcName string, service *Service) {
	operations := make(map[string]map[string]*openapi3.Operation) // map[path]map[method]*openapi3.Operation
	paths := make([]string, 0, len(a.methods[service]))
	methods := make([]string, 0, 7)

	for path, pathItems := range a.methods[service] { // map[path][]*openapi3.PathItem
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

					pp("// %s provides the %s", methType, summary)
				}

				// write service struct
				pp("type %s struct {", methType)
				pp("	s *Service")
				pp("	header http.Header")
				pp("	params url.Values")
				p("\n")

				// write path fields
				if len(pathParam) > 0 {
					pp("	// path fields")
					for _, param := range pathParam { // []*openapi3.ParameterRef
						paramName := NormalizeParam(Depunct(param.Value.Name, false))
						paramType, ok := typeConvMap[param.Value.Schema.Value.Type]
						if !ok {
							continue
						}

						pp("	%s %s", paramName, paramType)
					}
				}
				// write query fields
				if len(pm[openapi3.ParameterInQuery]) > 0 {
					pp("	// query fields")
					for _, param := range pm[openapi3.ParameterInQuery] { // []*openapi3.ParameterInQuery
						paramName := NormalizeParam(Depunct(param.Value.Name, false))
						paramType, ok := typeConvMap[param.Value.Schema.Value.Type]
						if !ok {
							continue
						}

						pp("	%s %s", paramName, paramType)
					}
				}
				pp("}")
				seen[methType] = true

				p("\n")

				// writes operation summary, if any
				if summary := strings.ToLower(op.Summary); summary != "" {
					// add dot if summary is not end to dot
					if summary[len(summary)-1] != '.' {
						summary += "."
					}

					pp("// %s returns the %s for %s", op.OperationID, methType, summary)
				}

				// write method
				p("func (r *%s) %s(", svcName, op.OperationID)
				if len(pathParam) > 0 {
					for i, param := range pathParam {
						p("%s %s", Depunct(param.Value.Name, false), param.Value.Schema.Value.Type)
						if i < len(pathParam)-1 {
							p(", ")
						}
					}
				}
				pp(") *%s {", methType)
				pp("	c := &%s{", methType)
				pp("		s: r.s,")
				pp("		header: make(http.Header),")
				pp("		params: url.Values{},")
				if len(pathParam) > 0 {
					for _, param := range pathParam {
						pp("		%[1]s: %[1]s,", Depunct(param.Value.Name, false))
					}
				}
				pp("	}")
				pp("	return c")
				pp("}")

				p("\n")

				// write query method chains
				for _, param := range pm[openapi3.ParameterInQuery] { // []*openapi3.Parameter
					paramName := NormalizeParam(Depunct(param.Value.Name, false))
					argName := Depunct(paramName, true)
					typeName := paramName
					pp("func (c *%[1]s) %[2]s(%[3]s %[4]s) *%[1]s {", methType, argName, typeName, typeConvMap[param.Value.Schema.Value.Type])
					pp("	c.params.Set(%[1]q, fmt.Sprintf(\"%%v\", %[1]s))", typeName)
					pp("	return c")
					pp("}")
					p("\n")
				}

				p("\n")

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
				methodType := "http.Method" + method

				// write request
				pp("// Do executes the %s.", svcName+op.OperationID)
				pp("func (c *%s) Do(ctx context.Context) (interface{}, error) {", methType)
				pp("	uri := path.Join(c.s.BasePath, \"%s\")", path)
				pp("	if len(c.params) > 0 {")
				pp("		uri += \"?\" + c.params.Encode()")
				pp("	}")
				p("\n")
				pp("	req, err := http.NewRequest(%s, uri, nil)", methodType)
				pp("	if err != nil { return nil, err }")
				p("\n")
				pp("	req.Header.Set(%q, %q)", hdrContentType, mimeJSON)
				pp("	req.Header.Set(%q, %q)", hdrAcceptEncoding, mimeJSON)
				p("\n")
				pp("	resp, err := c.s.client.Do(req.WithContext(ctx))")
				pp("	if err != nil { return nil, err }")
				pp("	defer resp.Body.Close()")
				p("\n")
				pp("	if resp.StatusCode != 200 {")
				pp("		return nil, errors.New(resp.Status)")
				pp("	}")
				p("\n")
				pp("	body, err := ioutil.ReadAll(resp.Body)")
				pp("	if err != nil { return nil, err }")
				p("\n")
				pp("	var result interface{}") // TODO(zchee): actual Response type
				pp("	if err := json.Unmarshal(body, &result); err != nil {")
				pp("		return nil, err")
				pp("	}")
				p("\n")
				pp("	return result, nil")
				pp("}")
			}
		}
	}
}

// WriteModel writes model definitions.
func (a *API) WriteModel(p, pp PrintFn, name string, component *openapi3.SchemaRef) {
	if component.Value != nil {
		// sort fields
		fields := make([]string, 0, len(component.Value.Properties))
		for field, _ := range component.Value.Properties {
			fields = append(fields, field)
		}
		sort.Strings(fields)

		p("// %s represents ", Depunct(name, true))
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
		pp(desc)

		fieldTypes := make(map[string]string)
		pp("type %s struct {", Depunct(name, true))
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
									pp("	%s []%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
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
								pp("	%s %s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
								fieldTypes[field] = typ
							}
						default:
							typ, ok := typeConvMap[objVal.Type]
							if ok {
								pp("	%s *%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
								fieldTypes[field] = typ
							}
						}
					} else {
						typ, ok := typeConvMap[val.Type]
						if ok {
							pp("	%s %s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
							fieldTypes[field] = typ
						}
					}
				case "array":
					if val.Items.Value != nil {
						typ, ok := typeConvMap[val.Items.Value.Type]
						if ok {
							pp("	%s []%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
							fieldTypes[field] = "[]" + typ
						}
					}
				default:
					typ, ok := typeConvMap[val.Type]
					if ok {
						pp("	%s *%s `json:\"%s,omitempty\"`", Depunct(field, true), typ, field)
						fieldTypes[field] = typ
					}
				}
			}
		}
		pp("}")

		for _, field := range fields {
			reciever := strings.ToLower(string(name[0]))
			fieldType := fieldTypes[field]
			if fieldType == "" {
				continue
			}
			field := Depunct(field, true)

			pp("// Get%[1]s returns the %[1]s field value if set, zero value otherwise.", field)
			pp("func (%s *%s) Get%s() (ret %s) {", reciever, Depunct(name, true), field, fieldType)
			pp(" 	if %[1]s == nil || %[1]s.%[2]s == nil {", reciever, field)
			pp(" 		return ret")
			pp(" 	}")
			// TODO(zchee): parse actual field type
			switch fieldType {
			case "map[string]interface{}", "interface{}":
				pp(" 	return %s.%s", reciever, field)
			default:
				p(" 	return ")
				if !strings.HasPrefix(fieldType, "[]") {
					p("*")
				}
				pp("%s.%s", reciever, field)
			}
			pp("}")

			p("\n")

			pp("// Has%s reports whether the field has been set", field)
			pp("func (%s *%s) Has%s() bool {", reciever, Depunct(name, true), field)
			pp(" 	if %[1]s != nil && %[1]s.%[2]s != nil {", reciever, field)
			pp(" 		return true")
			pp(" 	}")
			pp(" 	return false")
			pp("}")

			p("\n")

			pp("// Set%[1]s gets a reference to the given string and assigns it to the %[1]s field.", field)
			// TODO(zchee): parse actual field type
			switch {
			case fieldType == "map[string]interface{}", fieldType == "interface{}":
				pp("func (%s *%s) Set%s(val %s) {", reciever, Depunct(name, true), field, fieldType)
				pp("	%s.%s = val", reciever, field)
			default:
				if typ, ok := typeConvMap[fieldType]; ok {
					var hasPtr bool
					if !strings.HasPrefix(typ, "[]") {
						hasPtr = true
					}
					p("func (%s ", reciever)
					if hasPtr {
						p("*")
					}
					pp("%s) Set%s(val %s) {", Depunct(name, true), field, typ)

					p("	%s.%s = ", reciever, field)
					if hasPtr {
						p("&")
					}
					pp("val")
				}
			}
			pp("}")
		}
	}
}

// WriteSchemaDescriptor writes base64 encoded, gzipped compressed and JSON marshaled schema spec into generated file.
func (a *API) WriteSchemaDescriptor(p, pp PrintFn) {
	if a.openAPI == nil {
		return // not write
	}

	in := a.openAPI
	data, err := jsoniter.ConfigFastest.Marshal(in)
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

	pp("// SchemaDescriptor returns the Schema file descriptor which is generated code to this file.")
	pp("func SchemaDescriptor() (interface{}, error) {")
	pp("	zr, err := gzip.NewReader(bytes.NewReader(fileDescriptor))")
	pp("	if err != nil { return nil, err }")
	p("\n")
	pp("	var buf bytes.Buffer")
	pp("	_, err = buf.ReadFrom(zr)")
	pp("	if err != nil { return nil, err }")
	p("\n")
	pp("	var v interface{}")
	pp("	if err := json.NewDecoder(bytes.NewReader(buf.Bytes())).Decode(&v); err != nil {")
	pp("		return nil, err")
	pp("	}")
	p("\n")
	pp("	return v, nil")
	pp("}")

	p("\n")

	pp("// fileDescriptor gzipped JSON marshaled Schema object.")
	pp("var fileDescriptor = []byte{")
	pp("	// %d bytes of a gzipped Schema file descriptor", len(b))

	for len(b) > 0 {
		n := 16
		if n > len(b) {
			n = len(b)
		}

		s := ""
		for _, c := range b[:n] {
			s += fmt.Sprintf("0x%02x, ", c)
		}
		pp("	%s", s)

		b = b[n:]
	}
	pp("}")
}
