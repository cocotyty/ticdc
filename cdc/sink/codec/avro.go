// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package codec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"time"

	"github.com/linkedin/goavro/v2"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/tidb/types"
	tijson "github.com/pingcap/tidb/types/json"
	"go.uber.org/zap"
)

// AvroEventBatchEncoder converts the events to binary Avro data
type AvroEventBatchEncoder struct {
	// TODO use Avro for Kafka keys
	// keySchemaManager   *AvroSchemaManager
	valueSchemaManager *AvroSchemaManager
	keyBuf             []byte
	valueBuf           []byte
}

type avroEncodeResult struct {
	data       []byte
	registryID int
}

// NewAvroEventBatchEncoder creates an AvroEventBatchEncoder from an AvroSchemaManager
func NewAvroEventBatchEncoder(manager *AvroSchemaManager) *AvroEventBatchEncoder {
	return &AvroEventBatchEncoder{
		valueSchemaManager: manager,
		keyBuf:             nil,
		valueBuf:           nil,
	}
}

// AppendRowChangedEvent appends a row change event to the encoder
// NOTE: the encoder can only store one RowChangedEvent!
func (a *AvroEventBatchEncoder) AppendRowChangedEvent(e *model.RowChangedEvent) error {
	if a.keyBuf != nil || a.valueBuf != nil {
		return errors.New("Fatal sink bug. Batch size must be 1")
	}

	res, err := a.avroEncode(e.Table, e.SchemaID, e.Columns)
	if err != nil {
		log.Warn("AppendRowChangedEvent: avro encoding failed", zap.String("table", e.Table.String()))
		return errors.Annotate(err, "AppendRowChangedEvent could not encode to Avro")
	}

	evlp, err := res.toEnvelope()
	if err != nil {
		log.Warn("AppendRowChangedEvent: could not construct Avro envelope", zap.String("table", e.Table.String()))
		return errors.Annotate(err, "AppendRowChangedEvent could not construct Avro envelope")
	}

	a.valueBuf = evlp
	// TODO use primary key(s) as kafka key
	a.keyBuf = []byte(strconv.FormatInt(e.RowID, 10))

	return nil
}

// AppendResolvedEvent is no-op for now
func (a *AvroEventBatchEncoder) AppendResolvedEvent(ts uint64) error {
	// nothing for now
	return nil
}

// AppendDDLEvent generates new schema and registers it to the Registry
func (a *AvroEventBatchEncoder) AppendDDLEvent(e *model.DDLEvent) error {
	if e.ColumnInfo == nil {
		log.Info("AppendDDLEvent: no schema generation needed, skip")
		return nil
	}

	schemaStr, err := columnInfoToAvroSchema(e.Table, e.ColumnInfo)
	if err != nil {
		return errors.Annotate(err, "AppendDDLEvent failed")
	}
	log.Info("AppendDDLEvent: new schema generated", zap.String("schema_str", schemaStr))

	avroCodec, err := goavro.NewCodec(schemaStr)
	if err != nil {
		return errors.Annotate(err, "AppendDDLEvent failed: could not verify schema, probably bug")
	}

	err = a.valueSchemaManager.Register(context.Background(), model.TableName{
		Schema: e.Schema,
		Table:  e.Table,
	}, avroCodec)

	if err != nil {
		return errors.Annotate(err, "AppendDDLEvent failed: could not register schema")
	}

	return nil
}

// Build a MQ message
func (a *AvroEventBatchEncoder) Build() (key []byte, value []byte) {
	k := a.keyBuf
	v := a.valueBuf
	a.keyBuf = nil
	a.valueBuf = nil
	return k, v
}

// Size is always 0 or 1
func (a *AvroEventBatchEncoder) Size() int {
	if a.valueBuf == nil {
		return 0
	}
	return 1
}

func (a *AvroEventBatchEncoder) avroEncode(table *model.TableName, tiSchemaID int64, cols map[string]*model.Column) (*avroEncodeResult, error) {
	avroCodec, registryID, err := a.valueSchemaManager.Lookup(context.Background(), *table, tiSchemaID)
	if err != nil {
		return nil, errors.Annotate(err, "AvroEventBatchEncoder: lookup failed")
	}

	native, err := rowToAvroNativeData(cols)
	if err != nil {
		return nil, errors.Annotate(err, "AvroEventBatchEncoder: converting to native failed")
	}

	bin, err := avroCodec.BinaryFromNative(nil, native)
	if err != nil {
		return nil, errors.Annotate(err, "AvroEventBatchEncoder: converting to Avro binary failed")
	}

	return &avroEncodeResult{
		data:       bin,
		registryID: registryID,
	}, nil
}

type avroSchemaTop struct {
	Tp     string            `json:"type"`
	Name   string            `json:"name"`
	Fields []avroSchemaField `json:"fields"`
}

type avroSchemaField struct {
	Name         string      `json:"name"`
	Tp           []string    `json:"type"`
	DefaultValue interface{} `json:"default"`
}

func columnInfoToAvroSchema(name string, columnInfo []*model.ColumnInfo) (string, error) {
	top := avroSchemaTop{
		Tp:     "record",
		Name:   name,
		Fields: nil,
	}

	for _, col := range columnInfo {
		avroType, err := getAvroDataTypeNameMysql(col.Type)
		if err != nil {
			return "", err
		}

		field := avroSchemaField{
			Name:         col.Name,
			Tp:           []string{"null", avroType},
			DefaultValue: nil,
		}
		top.Fields = append(top.Fields, field)
	}

	str, err := json.Marshal(&top)
	if err != nil {
		return "", errors.Annotate(err, "columnInfoToAvroSchema: failed to generate json")
	}
	return string(str), nil
}

func rowToAvroNativeData(cols map[string]*model.Column) (interface{}, error) {
	ret := make(map[string]interface{}, len(cols))
	for key, col := range cols {
		data, str, err := columnToAvroNativeData(col)
		if err != nil {
			return nil, err
		}

		union := make(map[string]interface{}, 1)
		union[str] = data
		ret[key] = union
	}
	return ret, nil
}

func getAvroDataTypeName(v interface{}) (string, error) {
	switch v.(type) {
	case bool:
		return "boolean", nil
	case []byte:
		return "bytes", nil
	case float64:
		return "double", nil
	case float32:
		return "float", nil
	case int64, uint64:
		return "long", nil
	case int, int32, uint32:
		return "int", nil
	case nil:
		return "null", nil
	case string:
		return "string", nil
	case time.Duration:
		return "long.time-millis", nil
	case time.Time:
		return "long.timestamp-millis", nil
	default:
		log.Warn("getAvroDataTypeName: unknown type")
		return "", errors.New("unknown type for Avro")
	}
}

func getAvroDataTypeNameMysql(tp byte) (string, error) {
	switch tp {
	case mysql.TypeFloat:
		return "float", nil
	case mysql.TypeDouble:
		return "double", nil
	case mysql.TypeVarchar, mysql.TypeString, mysql.TypeVarString:
		return "string", nil
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeTimestamp:
		return "long.timestamp-millis", nil
	case mysql.TypeDuration: //duration should read fsp from column meta data
		return "long.time-millis", nil
	case mysql.TypeEnum:
		return "long", nil
	case mysql.TypeSet:
		return "long", nil
	case mysql.TypeBit:
		return "long", nil
	case mysql.TypeNewDecimal, mysql.TypeDecimal:
		return "string", nil
	case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24:
		return "int", nil
	case mysql.TypeLong, mysql.TypeLonglong:
		return "long", nil
	case mysql.TypeNull:
		return "null", nil
	case mysql.TypeJSON:
		return "string", nil
	case mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob, mysql.TypeBlob:
		return "bytes", nil
	default:
		return "", errors.New("Unknown Mysql type")
	}
}

func columnToAvroNativeData(col *model.Column) (interface{}, string, error) {
	if v, ok := col.Value.(int); ok {
		col.Value = int64(v)
	}

	switch col.Type {
	case mysql.TypeDate, mysql.TypeDatetime, mysql.TypeNewDate, mysql.TypeTimestamp:
		str := col.Value.(string)
		t, err := time.Parse(types.DateFormat, str)
		if err == nil {
			return t, "long.timestamp-millis", nil
		}

		t, err = time.Parse(types.TimeFormat, str)
		if err == nil {
			return t, "long.timestamp-millis", nil
		}

		t, err = time.Parse(types.TimeFSPFormat, str)
		if err != nil {
			return nil, "error", err
		}
		return t, "long.timestamp-millis", nil
	case mysql.TypeDuration:
		str := col.Value.(string)
		d, err := time.ParseDuration(str)
		if err != nil {
			return nil, "error", err
		}
		return d, "long.timestamp-millis", nil
	case mysql.TypeJSON:
		return col.Value.(tijson.BinaryJSON).String(), "string", nil
	case mysql.TypeNewDecimal, mysql.TypeDecimal:
		dec := col.Value.(*types.MyDecimal)
		if dec == nil {
			return nil, "null", nil
		}
		return dec.String(), "string", nil
	case mysql.TypeEnum:
		return col.Value.(types.Enum).Value, "long", nil
	case mysql.TypeSet:
		return col.Value.(types.Set).Value, "long", nil
	case mysql.TypeBit:
		return col.Value.(uint64), "long", nil
	case mysql.TypeTiny:
		return int32(col.Value.(uint8)), "int", nil
	default:
		avroType, err := getAvroDataTypeName(col.Value)
		if err != nil {
			return nil, "", err
		}
		return col.Value, avroType, nil
	}
}

const magicByte = uint8(0)

func (r *avroEncodeResult) toEnvelope() ([]byte, error) {
	buf := new(bytes.Buffer)
	data := []interface{}{magicByte, int32(r.registryID), r.data}
	for _, v := range data {
		err := binary.Write(buf, binary.LittleEndian, v)
		if err != nil {
			return nil, errors.Annotate(err, "converting Avro data to envelope failed")
		}
	}
	return buf.Bytes(), nil
}
