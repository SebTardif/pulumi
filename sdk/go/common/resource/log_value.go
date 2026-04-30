// Copyright 2026, Pulumi Corporation.
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

package resource

import (
	"encoding/binary"
	"log/slog"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// propertyValueLogMagic is the magic prefix for encoded property
// values: the ASCII string "pulumiPv" as a little-endian uint64.
// This must match the constant used by the Python and Node SDKs.
const propertyValueLogMagic uint64 = 0x7650696d756c7570

// logMarshalOpts are the MarshalOptions used when encoding property
// values for structured logging.
var logMarshalOpts = MarshalOptions{
	KeepSecrets:      true,
	KeepUnknowns:     true,
	KeepOutputValues: true,
}

// LogValue implements slog.LogValuer.  It encodes the property map
// as [8-byte magic][protobuf structpb.Value] bytes, matching the
// wire format used by the Python and Node SDK OTLP log emitters.
func (m PropertyMap) LogValue() slog.Value {
	s, err := MarshalProperties(m, logMarshalOpts)
	if err != nil {
		return slog.Value{}
	}
	sv := &structpb.Value{Kind: &structpb.Value_StructValue{StructValue: s}}
	return encodeLogValue(sv)
}

// LogValue implements slog.LogValuer.  See PropertyMap.LogValue for
// the wire format description.
func (v PropertyValue) LogValue() slog.Value {
	sv, err := MarshalPropertyValue("", v, logMarshalOpts)
	if err != nil {
		return slog.Value{}
	}
	return encodeLogValue(sv)
}

func encodeLogValue(sv *structpb.Value) slog.Value {
	valBytes, err := proto.Marshal(sv)
	if err != nil {
		return slog.Value{}
	}
	buf := make([]byte, 8+len(valBytes))
	binary.LittleEndian.PutUint64(buf[:8], propertyValueLogMagic)
	copy(buf[8:], valBytes)
	return slog.AnyValue(buf)
}
