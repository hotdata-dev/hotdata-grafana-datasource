package plugin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// MaxResultRows caps how many rows a single query result may accumulate in
// memory. Reading stops at the first record batch that reaches the cap, so a
// runaway SELECT cannot exhaust the plugin process; the caller surfaces the
// truncation to the user.
const MaxResultRows = 1_000_000

// FrameFromArrowStream decodes an Arrow IPC stream (as served by
// GET /v1/results/{id}?format=arrow) into a single Grafana data frame. The
// boolean reports whether the result was truncated at MaxResultRows.
//
// Hotdata's engine reports Arrow types directly (Utf8View, Int64, Float64,
// Timestamp, ...), so this mapping is the plugin's single source of type
// truth — the inline JSON preview from POST /v1/query is untyped and never
// used for data.
func FrameFromArrowStream(r io.Reader) (*data.Frame, bool, error) {
	rdr, err := ipc.NewReader(r)
	if err != nil {
		return nil, false, fmt.Errorf("opening arrow stream: %w", err)
	}
	defer rdr.Release()

	schema := rdr.Schema()
	fields := make([]*data.Field, len(schema.Fields()))
	appenders := make([]func(col arrow.Array) error, len(schema.Fields()))
	for i, f := range schema.Fields() {
		fields[i], appenders[i] = newFieldAppender(f)
	}

	rows := int64(0)
	truncated := false
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range fields {
			if err := appenders[i](rec.Column(i)); err != nil {
				return nil, false, fmt.Errorf("column %q: %w", schema.Field(i).Name, err)
			}
		}
		if rows += rec.NumRows(); rows >= MaxResultRows {
			// Whole batches are appended, so nothing was dropped yet even if the
			// crossing batch overshot the cap; the result is truncated only when
			// further batches remain unread.
			truncated = rdr.Next()
			break
		}
	}
	if err := rdr.Err(); err != nil && err != io.EOF {
		return nil, false, fmt.Errorf("reading arrow stream: %w", err)
	}

	return data.NewFrame("", fields...), truncated, nil
}

// newFieldAppender returns a nullable Grafana field for an Arrow schema field
// plus a function that appends one record batch's column to it.
func newFieldAppender(f arrow.Field) (*data.Field, func(col arrow.Array) error) {
	switch f.Type.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64:
		field := data.NewField(f.Name, nil, []*int64{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, intValue)
		}

	case arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
		// Kept unsigned so UInt64 values above math.MaxInt64 (hashes,
		// snowflake ids) don't wrap negative.
		field := data.NewField(f.Name, nil, []*uint64{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, uintValue)
		}

	case arrow.FLOAT16, arrow.FLOAT32, arrow.FLOAT64,
		arrow.DECIMAL128, arrow.DECIMAL256:
		field := data.NewField(f.Name, nil, []*float64{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, floatValue)
		}

	case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW:
		field := data.NewField(f.Name, nil, []*string{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, stringValue)
		}

	case arrow.BOOL:
		field := data.NewField(f.Name, nil, []*bool{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, boolValue)
		}

	case arrow.TIMESTAMP, arrow.DATE32, arrow.DATE64:
		field := data.NewField(f.Name, nil, []*time.Time{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, timeValue)
		}

	case arrow.BINARY, arrow.LARGE_BINARY:
		field := data.NewField(f.Name, nil, []*string{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, binaryValue)
		}

	default:
		// Lists, structs, maps, and anything unanticipated: JSON-encode via
		// the Arrow value's marshalable form (SPEC §7).
		field := data.NewField(f.Name, nil, []*string{})
		return field, func(col arrow.Array) error {
			return appendVia(field, col, func(col arrow.Array, i int) (string, error) {
				b, err := json.Marshal(col.GetOneForMarshal(i))
				if err != nil {
					return col.ValueStr(i), nil // fall back to text form
				}
				return string(b), nil
			})
		}
	}
}

// appendVia appends every row of col to field, mapping nulls to nil.
func appendVia[T any](field *data.Field, col arrow.Array, value func(col arrow.Array, i int) (T, error)) error {
	for i := 0; i < col.Len(); i++ {
		if col.IsNull(i) {
			field.Append(nil)
			continue
		}
		v, err := value(col, i)
		if err != nil {
			return err
		}
		field.Append(&v)
	}
	return nil
}

func intValue(col arrow.Array, i int) (int64, error) {
	switch c := col.(type) {
	case *array.Int8:
		return int64(c.Value(i)), nil
	case *array.Int16:
		return int64(c.Value(i)), nil
	case *array.Int32:
		return int64(c.Value(i)), nil
	case *array.Int64:
		return c.Value(i), nil
	}
	return 0, fmt.Errorf("unexpected integer array %T", col)
}

func uintValue(col arrow.Array, i int) (uint64, error) {
	switch c := col.(type) {
	case *array.Uint8:
		return uint64(c.Value(i)), nil
	case *array.Uint16:
		return uint64(c.Value(i)), nil
	case *array.Uint32:
		return uint64(c.Value(i)), nil
	case *array.Uint64:
		return c.Value(i), nil
	}
	return 0, fmt.Errorf("unexpected unsigned integer array %T", col)
}

func floatValue(col arrow.Array, i int) (float64, error) {
	switch c := col.(type) {
	case *array.Float16:
		return float64(c.Value(i).Float32()), nil
	case *array.Float32:
		return float64(c.Value(i)), nil
	case *array.Float64:
		return c.Value(i), nil
	case *array.Decimal128:
		return c.Value(i).ToFloat64(c.DataType().(*arrow.Decimal128Type).Scale), nil
	case *array.Decimal256:
		return c.Value(i).ToFloat64(c.DataType().(*arrow.Decimal256Type).Scale), nil
	}
	return 0, fmt.Errorf("unexpected float array %T", col)
}

func stringValue(col arrow.Array, i int) (string, error) {
	switch c := col.(type) {
	case *array.String:
		return c.Value(i), nil
	case *array.LargeString:
		return c.Value(i), nil
	case *array.StringView:
		return c.Value(i), nil
	}
	return "", fmt.Errorf("unexpected string array %T", col)
}

func boolValue(col arrow.Array, i int) (bool, error) {
	c, ok := col.(*array.Boolean)
	if !ok {
		return false, fmt.Errorf("unexpected bool array %T", col)
	}
	return c.Value(i), nil
}

func timeValue(col arrow.Array, i int) (time.Time, error) {
	switch c := col.(type) {
	case *array.Timestamp:
		unit := c.DataType().(*arrow.TimestampType).Unit
		return c.Value(i).ToTime(unit).UTC(), nil
	case *array.Date32:
		return c.Value(i).ToTime().UTC(), nil
	case *array.Date64:
		return c.Value(i).ToTime().UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unexpected time array %T", col)
}

func binaryValue(col arrow.Array, i int) (string, error) {
	switch c := col.(type) {
	case *array.Binary:
		return base64.StdEncoding.EncodeToString(c.Value(i)), nil
	case *array.LargeBinary:
		return base64.StdEncoding.EncodeToString(c.Value(i)), nil
	}
	return "", fmt.Errorf("unexpected binary array %T", col)
}
