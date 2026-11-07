// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginv1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// documentedConstants are the numeric constants of Plugin ABI v1
// (docs/architecture/05-wasm-plugin-system.md "Contract"), from which
// every PDK generates its own: a frozen ABI never renumbers them. The
// zero values of Err and Alg are the proto enum's own, no ABI constant.
func documentedConstants() map[string][]string {
	return map[string][]string{
		"Err": {
			"ERR_UNSPECIFIED=0", "ERR_ABSENT=-1", "ERR_PHASE=-2", "ERR_BUDGET=-3", "ERR_LIMIT=-4",
			"ERR_UNAVAILABLE=-5", "ERR_INVALID=-6", "ERR_DENIED=-7",
		},
		"Level": {"LEVEL_DEBUG=0", "LEVEL_INFO=1", "LEVEL_WARN=2", "LEVEL_ERROR=3"},
		"Alg":   {"ALG_UNSPECIFIED=0", "ALG_SHA_256=1", "ALG_SHA_384=2", "ALG_SHA_512=3"},
		"RequestInfoField": {
			"REQUEST_INFO_FIELD_METHOD=0", "REQUEST_INFO_FIELD_SCHEME=1", "REQUEST_INFO_FIELD_HOST=2",
			"REQUEST_INFO_FIELD_PATH=3", "REQUEST_INFO_FIELD_QUERY=4", "REQUEST_INFO_FIELD_SOURCE_ADDRESS=5",
			"REQUEST_INFO_FIELD_ROUTE=6",
		},
		"ConsumerInfoField": {
			"CONSUMER_INFO_FIELD_NAME=0", "CONSUMER_INFO_FIELD_TIER=1", "CONSUMER_INFO_FIELD_TAGS=2",
			"CONSUMER_INFO_FIELD_AUTH_METHOD=3", "CONSUMER_INFO_FIELD_CLAIMS=4",
		},
		"Direction": {"DIRECTION_CLIENT_TO_UPSTREAM=0", "DIRECTION_UPSTREAM_TO_CLIENT=1"},
	}
}

func TestDocumentedConstants(t *testing.T) {
	want := documentedConstants()
	got := map[string][]string{}
	for _, file := range []protoreflect.FileDescriptor{File_ruralz_plugin_v1_abi_proto, File_ruralz_plugin_v1_artifact_proto} {
		if file.Package() != "ruralz.plugin.v1" {
			t.Errorf("%s: package = %s, want ruralz.plugin.v1", file.Path(), file.Package())
		}
		if file.Services().Len() != 0 {
			t.Errorf("%s defines a service: no message of ruralz.plugin.v1 crosses the ABI", file.Path())
		}
		for i := range file.Enums().Len() {
			ed := file.Enums().Get(i)
			for j := range ed.Values().Len() {
				v := ed.Values().Get(j)
				got[string(ed.Name())] = append(got[string(ed.Name())], fmt.Sprintf("%s=%d", v.Name(), v.Number()))
			}
		}
	}
	for name, w := range want {
		if !slices.Equal(got[name], w) {
			t.Errorf("enum %s = %q, want %q", name, got[name], w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("enum %s is not a documented ABI constant", name)
		}
	}
	// The Go constants a host or PDK reads.
	if Err_ERR_ABSENT != -1 || Err_ERR_DENIED != -7 || Level_LEVEL_ERROR != 3 || Alg_ALG_SHA_512 != 3 ||
		RequestInfoField_REQUEST_INFO_FIELD_ROUTE != 6 || ConsumerInfoField_CONSUMER_INFO_FIELD_CLAIMS != 4 ||
		Direction_DIRECTION_UPSTREAM_TO_CLIENT != 1 {
		t.Error("a generated Go constant does not hold its documented value")
	}
}

func TestArtifactConfigFields(t *testing.T) {
	want := []string{
		"abi=1 string json:abi",
		"phases=2 repeated string json:phases",
		"capabilities=3 repeated string json:capabilities",
		"config_schema=4 google.protobuf.Struct json:configSchema",
		"pdk_version=5 string json:pdkVersion",
	}
	if n := File_ruralz_plugin_v1_artifact_proto.Messages().Len() + File_ruralz_plugin_v1_abi_proto.Messages().Len(); n != 1 {
		t.Errorf("ruralz.plugin.v1 has %d messages, want only the artifact config message", n)
	}
	fields := (&ArtifactConfig{}).ProtoReflect().Descriptor().Fields()
	var got []string
	for i := range fields.Len() {
		fd := fields.Get(i)
		typ := fd.Kind().String()
		if fd.Message() != nil {
			typ = string(fd.Message().FullName())
		}
		if fd.IsList() {
			typ = "repeated " + typ
		}
		got = append(got, fmt.Sprintf("%s=%d %s json:%s", fd.Name(), fd.Number(), typ, fd.JSONName()))
	}
	if !slices.Equal(got, want) {
		t.Errorf("ArtifactConfig fields = %q, want %q", got, want)
	}
}

// configBlob is an artifact config blob of media type
// application/vnd.ruralz.plugin.config.v1+json for the geo-block Plugin of
// docs/architecture/05-wasm-plugin-system.md "Plugin commands".
const configBlob = `{
  "abi": "ruralz.plugin.v1",
  "phases": ["onRequestHeaders"],
  "capabilities": ["request.headers.read", "response.send", "log.write"],
  "configSchema": {
    "type": "object",
    "required": ["denyCountries"],
    "properties": {"denyCountries": {"type": "array", "items": {"type": "string", "pattern": "^[A-Z]{2}$"}}},
    "x-ruralz-validations": [{"rule": "self.denyCountries.size() <= 50", "message": "at most 50 countries"}]
  },
  "pdkVersion": "0.1.0"
}`

func TestArtifactConfigJSON(t *testing.T) {
	var cfg ArtifactConfig
	if err := protojson.Unmarshal([]byte(configBlob), &cfg); err != nil {
		t.Fatalf("protojson.Unmarshal(config blob): %v", err)
	}
	if cfg.GetAbi() != "ruralz.plugin.v1" || !slices.Equal(cfg.GetPhases(), []string{"onRequestHeaders"}) ||
		!slices.Equal(cfg.GetCapabilities(), []string{"request.headers.read", "response.send", "log.write"}) ||
		cfg.GetPdkVersion() != "0.1.0" {
		t.Errorf("decoded config blob = %v", &cfg)
	}
	schema := cfg.GetConfigSchema().AsMap()
	if schema["type"] != "object" || schema["x-ruralz-validations"] == nil {
		t.Errorf("configSchema = %v, want the Plugin's JSON Schema with its x-ruralz-validations", schema)
	}

	// The JSON encoding round trips, as JSON: protojson's whitespace is
	// unstable by design, so only the parsed values compare.
	text, err := protojson.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var gotJSON, wantJSON any
	if err := json.Unmarshal(text, &gotJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(configBlob), &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Errorf("protojson.Marshal = %s, want the config blob %s", text, configBlob)
	}

	// And so does the binary encoding.
	wire, err := proto.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back ArtifactConfig
	if err := proto.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&back, &cfg) {
		t.Errorf("binary round trip = %v, want %v", &back, &cfg)
	}
}
