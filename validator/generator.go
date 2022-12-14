// Copyright 2022 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package validator

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/cloudwego/protoc-gen-validator/config"
	"github.com/cloudwego/protoc-gen-validator/parser"
	"github.com/cloudwego/protoc-gen-validator/util"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type Generator struct {
	*protogen.Plugin
	*protogen.GeneratedFile
	PbFile    *protogen.File
	config    *config.Config
	usedFuncs map[*template.Template]bool
}

func NewGenerator(plu *protogen.Plugin, file *protogen.File) (*Generator, error) {
	var cfg config.Config
	if err := cfg.Unpack(ParamsToArgs(plu.Request.GetParameter())); err != nil {
		return nil, fmt.Errorf("failed to unmarshal plugin parameters: %v", err)
	}
	return &Generator{
		Plugin:    plu,
		PbFile:    file,
		config:    &cfg,
		usedFuncs: make(map[*template.Template]bool),
	}, nil
}

// Pf wraps P() for format write
func (g *Generator) Pf(format string, a ...interface{}) {
	g.P(fmt.Sprintf(format, a...))
}

func (g *Generator) Generate() error {
	var err error
	filename := g.PbFile.GeneratedFilenamePrefix + "_validate.pb.go"
	genFile := g.NewGeneratedFile(filename, g.PbFile.GoImportPath)
	g.GeneratedFile = genFile
	g.generateHeader()
	g.generatePackage()
	g.generateImportAndGuard()
	err = g.generateValidate()
	g.generateFuncsImport()
	if err != nil {
		return err
	}
	return nil
}

func (g *Generator) generateImportAndGuard() {
	for _, impt := range []protogen.GoIdent{
		{
			GoName:       "bytes",
			GoImportPath: "bytes",
		},
		{
			GoName:       "fmt",
			GoImportPath: "fmt",
		},
		{
			GoName:       "reflect",
			GoImportPath: "reflect",
		},
		{
			GoName:       "regexp",
			GoImportPath: "regexp",
		},
		{
			GoName:       "strings",
			GoImportPath: "strings",
		},
		{
			GoName:       "time",
			GoImportPath: "time",
		},
	} {
		g.QualifiedGoIdent(impt)
	}
	g.P("// unused protection")
	g.P("var (")
	g.P("_ = fmt.Formatter(nil)")
	g.P("_ = (*bytes.Buffer)(nil)")
	g.P("_ = (*strings.Builder)(nil)")
	g.P("_ = reflect.Type(nil)")
	g.P("_ = (*regexp.Regexp)(nil)")
	g.P("_ = time.Nanosecond")
	g.P(")")
	g.P()
}

func (g *Generator) generateValidate() error {
	for _, st := range g.PbFile.Messages {
		vcs, err := mkMsgValidateContext(st, g.PbFile)
		if err != nil {
			return err
		}
		g.Pf("func (m *%s)Validate() error {", st.GoIdent.GoName)
		for _, vc := range vcs {
			switch vc.ValidationType {
			case parser.StructLikeValidation:
				if len(vc.Rules) == 0 {
					continue
				}
				if err = g.generateStructLikeValidation(vc); err != nil {
					return err
				}
			default:
				if len(vc.Rules) == 0 {
					continue
				}
				if err = g.generateFieldValidation(vc, false); err != nil {
					return err
				}
			}
		}
		g.P("return nil")
		g.P("}")
		g.P()
	}

	return nil
}

func (g *Generator) generateHeader() {
	g.P("// Code generated by protoc-gen-validator. DO NOT EDIT.")
	g.P("// versions:")
	g.Pf("// \tprotoc-gen-validator %s", Version)
	g.P("// source: ", g.PbFile.Desc.Path())
	g.P()
}

func (g *Generator) generatePackage() {
	g.P("package ", g.PbFile.GoPackageName)
	g.P()
}

func (g *Generator) generateFieldValidation(vc *ValidateContext, isInnerType bool) error {
	for _, r := range vc.Rules {
		if r.Key == parser.NotNil && r.Specified.TypedValue.Bool {
			g.P(fmt.Sprintf("if m.%s == nil {", vc.FieldName))
			g.P(fmt.Sprintf("return fmt.Errorf(\"field %s not_nil rule failed\")\n", vc.RawFieldName))
			g.P("}")
		}
	}

	var err error
	if vc.RawField.Desc.IsList() && !isInnerType {
		return g.generateListValidation(vc)
	}

	if vc.RawField.Desc.IsMap() && !isInnerType {
		return g.generateMapValidation(vc)
	}

	if vc.RawField.Desc.Kind() == protoreflect.MessageKind {
		err = g.generateStructLikeFieldValidation(vc)
	} else if vc.RawField.Desc.Kind() == protoreflect.EnumKind {
		err = g.generateEnumValidation(vc)
	} else {
		err = g.generateBaseTypeValidation(vc)
	}
	if err != nil {
		return err
	}
	return nil
}

func (g *Generator) generateEnumValidation(vc *ValidateContext) error {
	var target, source string
	for _, rule := range vc.Rules {
		// construct target
		target = vc.GetNameFunc
		enumNameMap := util.CamelCase(string(vc.RawField.Desc.Enum().Name())) + "_name" // enumType_name is generated by protoc-gen-go
		// construct source
		switch rule.Key {
		case parser.Const:
			enumConst, err := g.getEnumValue(rule, vc)
			if err != nil {
				return err
			}
			if vc.RawField.Enum != nil {
				g.QualifiedGoIdent(vc.RawField.Enum.GoIdent)
			}
			source = vc.GenID("_src")
			g.Pf("%s := %s", source, enumConst)
		case parser.DefinedOnly,
			parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown bool annotation")
		}
		// generate validation code
		switch rule.Key {
		case parser.Const:
			g.Pf("if %s != %s {", target, source)
			g.Pf("return fmt.Errorf(\"field %s const rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.DefinedOnly:
			if rule.Specified.TypedValue.Bool {
				g.Pf("if _, ok := %s[int32(%s)]; !ok {", enumNameMap, target)
				g.Pf("return fmt.Errorf(\"field %s defined_only rule failed\")", vc.RawFieldName)
				g.P("}")
			}
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown enum annotation")
		}
	}
	return nil
}

func (g *Generator) getEnumValue(rule *parser.Rule, vc *ValidateContext) (string, error) {
	identifier := rule.Specified.TypedValue.Binary
	divId := strings.Split(identifier, ".")
	switch len(divId) {
	case 2: // enumType.enumValue
		return g.getCurPackageEnumValue(divId, vc)
	case 3: // import.enumType.enumValue
		return g.getDepPackageEnumValue(divId, vc)
	default:
		return "", fmt.Errorf("wrong format for enum rule: %s", identifier)
	}
}

func (g *Generator) getCurPackageEnumValue(divId []string, vc *ValidateContext) (string, error) {
	curPackage := vc.PbFile.Proto.GetPackage()
	for _, file := range g.Files {
		if file.Proto.GetPackage() == curPackage {
			for _, enum := range file.Enums {
				if string(enum.Desc.Name()) == divId[0] {
					for _, enumVal := range enum.Values {
						if string(enumVal.Desc.Name()) == divId[1] {
							return enumVal.GoIdent.GoName, nil
						}
					}
				}
			}
		}
	}
	return "", fmt.Errorf("can not find enum value '%s.%s' in package '%s'", divId[0], divId[1], curPackage)
}

func (g *Generator) getDepPackageEnumValue(divId []string, vc *ValidateContext) (string, error) {
	for _, file := range g.Files {
		if file.Proto.GetPackage() == divId[0] {
			for _, enum := range file.Enums {
				if string(enum.Desc.Name()) == divId[1] {
					for _, enumVal := range enum.Values {
						if string(enumVal.Desc.Name()) == divId[2] {
							return string(file.GoPackageName) + "." + enumVal.GoIdent.GoName, nil
						}
					}
				}
			}
		}
	}

	return "", fmt.Errorf("can not find enum value '%s.%s' in package '%s'", divId[1], divId[2], divId[0])
}

func (g *Generator) generateBaseTypeValidation(vc *ValidateContext) error {
	switch vc.RawField.Desc.Kind() {
	case protoreflect.BoolKind:
		return g.generateBoolValidation(vc)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		return g.generateNumericValidation(vc)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return g.generateNumericValidation(vc)
	case protoreflect.StringKind, protoreflect.BytesKind:
		return g.generateBinaryValidation(vc)
	default:
		return errors.New("unknown base annotation")
	}
}

func (g *Generator) generateNumericValidation(vc *ValidateContext) error {
	var target, source, typeName string
	for _, rule := range vc.Rules {
		// construct target
		target = vc.GetNameFunc
		typeName, _ = fieldGoType(g.GeneratedFile, vc.RawField)
		// construct source
		switch rule.Key {
		case parser.Const, parser.LessThan, parser.LessEqual, parser.GreatThan, parser.GreatEqual:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.IntValue:
				source = strconv.FormatInt(vt.TypedValue.Int, 10)
			case parser.DoubleValue:
				source = strconv.FormatFloat(vt.TypedValue.Double, 'f', -1, 64)
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			case parser.FunctionValue:
				source = vc.GenID("_src")
				if err := g.generateFunction(source, vc, vt.TypedValue.Function); err != nil {
					return err
				}
				g.P()
			default:
				return fmt.Errorf("unsupported value type for %s in numeric validation", parser.KeyString[rule.Key])
			}
		case parser.In, parser.NotIn:
			source = vc.GenID("_src")
			err := g.generateSlice(source, vc, rule.Range)
			if err != nil {
				return err
			}
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown numeric annotation")
		}

		switch rule.Key {
		case parser.Const:
			g.Pf("if %s != %s(%s) {", target, typeName, source)
			g.Pf("return fmt.Errorf(\"field %s not match const value, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.LessThan:
			g.Pf("if %s >= %s(%s) {", target, typeName, source)
			g.Pf("return fmt.Errorf(\"field %s lt rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.LessEqual:
			g.Pf("if %s > %s(%s) {", target, typeName, source)
			g.Pf("return fmt.Errorf(\"field %s le rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.GreatThan:
			g.Pf("if %s <= %s(%s) {", target, typeName, source)
			g.Pf("return fmt.Errorf(\"field %s gt rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.GreatEqual:
			g.Pf("if %s < %s(%s) {", target, typeName, source)
			g.Pf("return fmt.Errorf(\"field %s ge rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.In:
			exist := vc.GenID("_exist")
			g.Pf("var %s bool", exist)
			g.Pf("for _, src := range %s {", source)
			g.Pf("if %s == %s(src) {", target, typeName)
			g.Pf("%s = true", exist)
			g.P("break")
			g.P("}")
			g.P("}")
			g.Pf("if !%s {", exist)
			g.Pf("return fmt.Errorf(\"field %s in rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.NotIn:
			g.Pf("for _, src := range %s {", source)
			g.Pf("if %s == %s(src) {", target, typeName)
			g.Pf("return fmt.Errorf(\"field %s not_in rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
			g.P("}")
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown numeric annotation")
		}
	}
	return nil
}

func (g *Generator) generateSlice(name string, vc *ValidateContext, vals []*parser.ValidationValue) error {
	if len(vals) == 0 {
		return errors.New("empty validation values")
	}
	typeID := vc.RawField.Desc.Kind().String()
	goType, _ := fieldGoType(g.GeneratedFile, vc.RawField)
	str := strings.Builder{}
	str.WriteString(fmt.Sprintf("%s := []", name))
	var vs []string
	if IsBaseType(vc.RawField) {
		str.WriteString(fmt.Sprintf("%s{", goType))
	} else {
		return fmt.Errorf("type %s not supported in generate slice", typeID)
	}

	switch typeID {
	case "int32", "sint32", "uint32", "int64", "sint64", "uint64",
		"sfixed32", "fixed32", "sfixed64", "fixed64":
		for _, val := range vals {
			var source string
			if val.ValueType == parser.FieldReferenceValue {
				source = val.TypedValue.GetFieldReferenceName("m.")
			} else {
				source = strconv.FormatInt(val.TypedValue.Int, 10)
			}
			vs = append(vs, goType+"("+source+")")
		}
	case "float", "double":
		for _, val := range vals {
			var source string
			if val.ValueType == parser.FieldReferenceValue {
				source = val.TypedValue.GetFieldReferenceName("m.")
			} else {
				source = strconv.FormatFloat(val.TypedValue.Double, 'f', -1, 64)
			}
			vs = append(vs, goType+"("+source+")")
		}
	case "string":
		for _, val := range vals {
			var source string
			if val.ValueType == parser.FieldReferenceValue {
				source = val.TypedValue.GetFieldReferenceName("m.")
			} else {
				source = "\"" + val.TypedValue.Binary + "\""
			}
			vs = append(vs, goType+"("+source+")")
		}
	case "bytes":
		for _, val := range vals {
			var source string
			if val.ValueType == parser.FieldReferenceValue {
				source = val.TypedValue.GetFieldReferenceName("m.")
			} else {
				source = "\"" + val.TypedValue.Binary + "\""
			}
			vs = append(vs, goType+"("+source+")")
		}
	default:
		return fmt.Errorf("type %s not supported in generate slice", typeID)
	}
	str.WriteString(fmt.Sprintf("%s}\n", strings.Join(vs, ", ")))
	g.P(str.String())
	return nil
}

func (g *Generator) generateBoolValidation(vc *ValidateContext) error {
	var target, source string
	for _, rule := range vc.Rules {
		// construct target
		target = vc.GetNameFunc
		// construct source
		switch rule.Key {
		case parser.Const:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.BoolValue:
				source = strconv.FormatBool(vt.TypedValue.Bool)
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			}
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown bool annotation")
		}
		// generate validation code
		switch rule.Key {
		case parser.Const:
			g.Pf("if %s != %s {", target, source)
			g.Pf("return fmt.Errorf(\"field %s const rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.NotNil:
			// nothing
		default:
			return errors.New("unknown bool annotation")
		}
	}
	return nil
}

func (g *Generator) generateBinaryValidation(vc *ValidateContext) error {
	var target, source string
	for _, rule := range vc.Rules {
		// construct target
		target = vc.GetNameFunc
		// construct source
		switch rule.Key {
		case parser.Const, parser.Prefix, parser.Suffix, parser.Contains, parser.NotContains, parser.Pattern:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			case parser.FunctionValue:
				source = vc.GenID("_src")
				if err := g.generateFunction(source, vc, vt.TypedValue.Function); err != nil {
					return err
				}
			default:
				source = vc.GenID("_src")
				if vc.RawField.Desc.Kind().String() == "string" || rule.Key == parser.Pattern {
					g.P(source + " := \"" + vt.TypedValue.Binary + "\"")
				} else {
					g.P(source + " := []byte(\"" + vt.TypedValue.Binary + "\")")
				}
			}
		case parser.MinSize, parser.MaxSize:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			case parser.IntValue:
				source = strconv.FormatInt(vt.TypedValue.Int, 10)
			case parser.FunctionValue:
				source = vc.GenID("_src")
				if err := g.generateFunction(source, vc, vt.TypedValue.Function); err != nil {
					return err
				}
			}
		case parser.In, parser.NotIn:
			source = vc.GenID("_src")
			g.generateSlice(source, vc, rule.Range)
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown binary annotation")
		}
		// generate validation code
		switch rule.Key {
		case parser.MinSize:
			g.Pf("if len(%s) < int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s min_len rule failed, current value: %%d\", len(%s))", vc.RawFieldName, target)
			g.P("}")
		case parser.MaxSize:
			g.Pf("if len(%s) > int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s max_len rule failed, current value: %%d\", len(%s))", vc.RawFieldName, target)
			g.P("}")
		case parser.Const:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if %s != %s {", target, source)
			} else {
				g.Pf("if !bytes.Equal(%s, %s) {", target, source)
			}
			g.Pf("return fmt.Errorf(\"field %s not match const value, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.Prefix:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if !strings.HasPrefix(%s, %s) {", target, source)
			} else {
				g.Pf("if !bytes.HasPrefix(%s, %s) {", target, source)
			}
			g.Pf("return fmt.Errorf(\"field %s prefix rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.Suffix:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if !strings.HasSuffix(%s, %s) {", target, source)
			} else {
				g.Pf("if !bytes.HasSuffix(%s, %s) {", target, source)
			}
			g.Pf("return fmt.Errorf(\"field %s suffix rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.Contains:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if !strings.Contains(%s, %s) {", target, source)
			} else {
				g.Pf("if !bytes.Contains(%s, %s) {", target, source)
			}
			g.Pf("return fmt.Errorf(\"field %s contains rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.NotContains:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if strings.Contains(%s, %s) {", target, source)
			} else {
				g.Pf("if bytes.Contains(%s, %s) {", target, source)
			}
			g.Pf("return fmt.Errorf(\"field %s not_contains rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.Pattern:
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if ok, _ := regexp.MatchString(%s, %s); !ok {", source, target)
			} else {
				g.Pf("if ok, _ := regexp.Match(string(%s), %s); !ok {", source, target)
			}
			g.Pf("return fmt.Errorf(\"field %s pattern rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.In:
			exist := vc.GenID("_exist")
			g.Pf("var %s bool", exist)
			g.Pf("for _, src := range %s {", source)
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if %s == src {", target)
			} else {
				g.Pf("if bytes.Equal(%s, src) {", target)
			}
			g.Pf("%s = true", exist)
			g.P("break")
			g.P("}")
			g.P("}")
			g.Pf("if !%s {", exist)
			g.Pf("return fmt.Errorf(\"field %s in rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.NotIn:
			g.Pf("for _, src := range %s {", source)
			if vc.RawField.Desc.Kind().String() == "string" {
				g.Pf("if %s == src {", target)
			} else {
				g.Pf("if bytes.Equal(%s, src) {", target)
			}
			g.Pf("return fmt.Errorf(\"field %s not_in rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
			g.P("}")
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown binary annotation")
		}
	}
	return nil
}

func (g *Generator) generateStructLikeFieldValidation(vc *ValidateContext) error {
	var skip bool
	for _, rule := range vc.Rules {
		switch rule.Key {
		case parser.Skip:
			if rule.Specified.TypedValue.Bool {
				g.Pf("// skip field %s check", vc.RawFieldName)
				skip = true
			}
		case parser.NotNil:
			// do nothing
		default:
			return errors.New("unknown struct like annotation")
		}
	}
	if !skip {
		g.Pf("if err := %s.Validate(); err != nil {", vc.GetNameFunc)
		g.Pf("return fmt.Errorf(\"filed %s not valid, %%w\", err)", vc.RawFieldName)
		g.P("}")
	}
	return nil
}

func (g *Generator) generateListValidation(vc *ValidateContext) error {
	var target, source string
	target = vc.GetNameFunc
	for _, rule := range vc.Rules {
		switch rule.Key {
		case parser.MinSize, parser.MaxSize:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.IntValue:
				source = strconv.FormatInt(vt.TypedValue.Int, 10)
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			case parser.FunctionValue:
				source = vc.GenID("_src")
				if err := g.generateFunction(source, vc, vt.TypedValue.Function); err != nil {
					return err
				}
			}
		case parser.Elem:
			// do nothing
		default:
			return errors.New("unknown list annotation")
		}
		switch rule.Key {
		case parser.MinSize:
			g.Pf("if len(%s) < int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s MinLen rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.MaxSize:
			g.Pf("if len(%s) > int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s MaxLen rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.Elem:
			g.Pf("for i := 0; i < len(%s); i++ {", target)
			elemName := vc.GenID("_elem")
			g.Pf("%s := %s[i]", elemName, target)

			// generate inner validate rule, so create a new ValidateContext
			vt := &ValidateContext{
				RawField:     vc.RawField,
				PbFile:       vc.PbFile,
				FieldName:    elemName,
				RawFieldName: elemName,
				GetNameFunc:  elemName,
				Msg:          vc.Msg,
				Validation:   rule.Inner,
				ids:          vc.ids,
			}
			if err := g.generateFieldValidation(vt, true); err != nil {
				return err
			}
			g.P("}")
		default:
			return errors.New("unknown list annotation")
		}
	}
	return nil
}

func (g *Generator) generateMapValidation(vc *ValidateContext) error {
	var target, source string
	target = vc.GetNameFunc
	for _, rule := range vc.Rules {
		switch rule.Key {
		case parser.MinSize, parser.MaxSize:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.IntValue:
				source = strconv.FormatInt(vt.TypedValue.Int, 10)
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			case parser.FunctionValue:
				source = vc.GenID("_src")
				if err := g.generateFunction(source, vc, vt.TypedValue.Function); err != nil {
					return err
				}
			}
		case parser.NoSparse:
			vt := rule.Specified
			switch vt.ValueType {
			case parser.BoolValue:
				source = strconv.FormatBool(vt.TypedValue.Bool)
			case parser.FieldReferenceValue:
				source = vt.TypedValue.GetFieldReferenceName("m.")
			}
		case parser.MapKey, parser.MapValue:
			// do nothing
		default:
			return errors.New("unknown map annotation")
		}
		switch rule.Key {
		case parser.MinSize:
			g.Pf("if len(%s) < int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s min_size rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.MaxSize:
			g.Pf("if len(%s) > int(%s) {", target, source)
			g.Pf("return fmt.Errorf(\"field %s max_size rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
		case parser.NoSparse:
			if vc.RawField.Desc.MapValue().Kind() != protoreflect.MessageKind {
				return fmt.Errorf("field %s: no_sparse rule is only applicable for embedded message types", vc.RawFieldName)
			}
			g.Pf("for _, v := range %s {", target)
			g.Pf("if v == nil {")
			g.Pf("return fmt.Errorf(\"field %s no_sparse rule failed, current value: %%v\", %s)", vc.RawFieldName, target)
			g.P("}")
			g.P("}")
		case parser.MapKey:
			g.Pf("for k := range %s {", target)

			// transfer map key field desc to protogen.Field
			keyField := &protogen.Field{
				Desc: vc.RawField.Desc.MapKey(),
			}
			// transfer map key for desc to protogen.File for base type
			fileField := &protogen.File{
				Desc: vc.RawField.Desc.ParentFile(),
			}
			// for non-base type (enum/message)
			if vc.RawField.Desc.MapValue().Kind() == protoreflect.MessageKind ||
				vc.RawField.Desc.MapValue().Kind() == protoreflect.EnumKind {
				fileField = &protogen.File{
					Desc: vc.RawField.Desc.MapValue().ParentFile(),
				}
				if vc.RawField.Desc.MapValue().Kind() == protoreflect.EnumKind {
					fileProto, err := g.getEnumFileDescriptorProto(vc.RawField)
					if err != nil {
						return err
					}
					fileField.Proto = fileProto
				}
			}

			vt := &ValidateContext{
				FieldName:    "k",
				RawFieldName: "k",
				GetNameFunc:  "k",
				Validation:   rule.Inner,
				ids:          vc.ids,
				RawField:     keyField,
				PbFile:       fileField,
				Msg:          nil,
			}
			if err := g.generateFieldValidation(vt, true); err != nil {
				return err
			}
			g.P("}")
		case parser.MapValue:
			g.Pf("for _, v := range %s {", target)
			// transfer map value field desc to protogen.Field
			valueField := &protogen.Field{
				Desc: vc.RawField.Desc.MapValue(),
			}
			if vc.RawField.Desc.MapValue().Kind() == protoreflect.EnumKind {
				enumDes, err := g.getEnumEnumDescriptorProto(vc.RawField)
				if err != nil {
					return err
				}
				valueField.Enum = enumDes
				g.QualifiedGoIdent(enumDes.GoIdent)
			}

			// transfer map value for desc to protogen.File for base type
			fileField := &protogen.File{
				Desc: vc.RawField.Desc.ParentFile(),
			}
			// for non-base type (enum/message)
			if vc.RawField.Desc.MapValue().Kind() == protoreflect.MessageKind ||
				vc.RawField.Desc.MapValue().Kind() == protoreflect.EnumKind {
				fileField = &protogen.File{
					Desc: vc.RawField.Desc.MapValue().ParentFile(),
				}
				if vc.RawField.Desc.MapValue().Kind() == protoreflect.EnumKind {
					fileProto, err := g.getEnumFileDescriptorProto(vc.RawField)
					if err != nil {
						return err
					}
					fileField.Proto = fileProto
				}
			}

			vt := &ValidateContext{
				FieldName:    "v",
				RawFieldName: "v",
				GetNameFunc:  "v",
				Validation:   rule.Inner,
				ids:          vc.ids,
				RawField:     valueField,
				PbFile:       fileField,
				Msg:          nil,
			}
			if err := g.generateFieldValidation(vt, true); err != nil {
				return err
			}
			g.P("}")
		default:
			return errors.New("unknown map annotation")
		}
	}
	return nil
}

func (g *Generator) getEnumFileDescriptorProto(rawField *protogen.Field) (*descriptorpb.FileDescriptorProto, error) {
	for _, file := range g.Files {
		for _, enum := range file.Enums {
			if enum.Desc.Name() == rawField.Desc.MapValue().Enum().Name() && file.Desc.Package() == rawField.Desc.MapValue().Enum().ParentFile().Package() {
				return file.Proto, nil
			}
		}
	}

	return nil, fmt.Errorf("can not find enum: %s defination in all file", rawField.Desc.Name())
}

func (g *Generator) getEnumEnumDescriptorProto(rawField *protogen.Field) (*protogen.Enum, error) {
	for _, file := range g.Files {
		for _, enum := range file.Enums {
			if enum.Desc.Name() == rawField.Desc.MapValue().Enum().Name() && file.Desc.Package() == rawField.Desc.MapValue().Enum().ParentFile().Package() {
				return enum, nil
			}
		}
	}

	return nil, fmt.Errorf("can not find enum: %s defination in all file", rawField.Desc.Name())
}

func (g *Generator) generateFunction(source string, vc *ValidateContext, f *parser.ToolFunction) error {
	switch f.Name {
	case "len":
		g.Pf(source+" := len(%s)", f.Arguments[0].TypedValue.GetFieldReferenceName("m."))
	case "sprintf":
		str := strings.Builder{}
		str.WriteString(source + " := fmt.Sprintf(")
		var args []string
		for _, arg := range f.Arguments {
			switch arg.ValueType {
			case parser.BinaryValue:
				args = append(args, "\""+arg.TypedValue.Binary+"\"")
			case parser.FieldReferenceValue:
				args = append(args, arg.TypedValue.GetFieldReferenceName("m."))
			}
		}
		str.WriteString(strings.Join(args, ",") + ")")
		g.P(str.String())
	// binary function
	case "equal", "mod", "add":
		var args []string
		str := strings.Builder{}
		for _, arg := range f.Arguments {
			argName, err := g.renderValidationValue(vc, &arg)
			if err != nil {
				return err
			}
			args = append(args, argName)
		}
		if len(args) < 2 {
			return fmt.Errorf("binary function %s needs at least 2 arguments", f.Name)
		}
		str.WriteString(source + " := " + args[0])
		switch f.Name {
		case "equal":
			str.WriteString(" == ")
		case "mod":
			str.WriteString(" % ")
		case "add":
			str.WriteString(" + ")
		}
		str.WriteString(args[1])
		g.P(str.String())
	case "now_unix_nano":
		g.Pf(source + ":= time.Now().UnixNano()")
		return nil
	default:
		funcTemplate := g.config.GetFunction(f.Name)
		if funcTemplate == nil {
			return errors.New("unknown function: " + f.Name)
		}
		var buf bytes.Buffer
		err := funcTemplate.Execute(&buf, &struct {
			Source     string
			StructLike *protogen.File
			Function   *parser.ToolFunction
		}{
			Source:     source,
			StructLike: vc.PbFile,
			Function:   f,
		})
		if err != nil {
			return fmt.Errorf("execute function %s's template failed: %v", f.Name, err)
		}
		g.P(buf.String())
		g.usedFuncs[funcTemplate] = true
	}
	return nil
}

func (g *Generator) renderValidationValue(vc *ValidateContext, val *parser.ValidationValue) (string, error) {
	switch val.ValueType {
	case parser.DoubleValue:
		return fmt.Sprintf("float64(%f)", val.TypedValue.Double), nil
	case parser.IntValue:
		return fmt.Sprintf("int64(%d)", val.TypedValue.Int), nil
	case parser.FunctionValue:
		source := vc.GenID("_src")
		g.generateFunction(source, vc, val.TypedValue.Function)
		return source, nil
	case parser.FieldReferenceValue:
		return val.TypedValue.GetFieldReferenceName("m."), nil
	default:
		return "", fmt.Errorf("value type %s is not supported for equal", val.ValueType)
	}
}

func (g *Generator) generateFuncsImport() {
	var importBuf bytes.Buffer
	for tpl := range g.usedFuncs {
		if strings.Contains(tpl.DefinedTemplates(), "Import") {
			if err := tpl.ExecuteTemplate(&importBuf, "Import", g.PbFile); err != nil {
				log.Printf(fmt.Sprintf("failed to Imports template of %s, err: %v", tpl.Name(), err))
			}
		}
	}
	if importBuf.Len() == 0 {
		return
	}
	importStr := strings.TrimSpace(importBuf.String())
	importStr = strings.ReplaceAll(importStr, "\n\n", "\n")
	importSplit := strings.Split(importStr, "\n")
	for _, impt := range importSplit {
		goIdent := protogen.GoIdent{
			GoName:       filepath.Base(impt[1 : len(impt)-1]),
			GoImportPath: protogen.GoImportPath(impt[1 : len(impt)-1]),
		}
		g.QualifiedGoIdent(goIdent)
	}
}

func (g *Generator) generateStructLikeValidation(vc *ValidateContext) error {
	for _, rule := range vc.Rules {
		switch rule.Key {
		case parser.Assert:
			source := vc.GenID("_assert")
			err := g.generateFunction(source, vc, rule.Specified.TypedValue.Function)
			if err != nil {
				return err
			}
			g.Pf("if !(" + source + ") {")
			g.P("return fmt.Errorf(\"struct assertion failed\")")
			g.P("}")
		default:
			return errors.New("unknown struct like annotation")
		}
	}
	return nil
}