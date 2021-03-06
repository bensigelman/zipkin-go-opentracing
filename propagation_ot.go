package zipkintracer

import (
	"encoding/binary"
	"io"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/proto"
	opentracing "github.com/opentracing/opentracing-go"

	"github.com/openzipkin/zipkin-go-opentracing/flag"
	"github.com/openzipkin/zipkin-go-opentracing/wire"
)

type textMapPropagator struct {
	tracer *tracerImpl
}
type binaryPropagator struct {
	tracer *tracerImpl
}

const (
	prefixBaggage = "ot-baggage-"

	tracerStateFieldCount = 3 // not 5, X-B3-ParentSpanId is optional and we allow optional Sampled header

	zipkinTraceID      = "X-B3-TraceId"
	zipkinSpanID       = "X-B3-SpanId"
	zipkinParentSpanID = "X-B3-ParentSpanId"
	zipkinSampled      = "X-B3-Sampled"
	zipkinFlags        = "X-B3-Flags"

	zipkinTraceIDLower      = "x-b3-traceid"
	zipkinSpanIDLower       = "x-b3-spanid"
	zipkinParentSpanIDLower = "x-b3-parentspanid"
	zipkinSampledLower      = "x-b3-sampled"
	zipkinFlagsLower        = "x-b3-flags"
)

func (p *textMapPropagator) Inject(
	spanContext opentracing.SpanContext,
	opaqueCarrier interface{},
) error {
	sc, ok := spanContext.(*SpanContext)
	if !ok {
		return opentracing.ErrInvalidSpanContext
	}
	carrier, ok := opaqueCarrier.(opentracing.TextMapWriter)
	if !ok {
		return opentracing.ErrInvalidCarrier
	}

	carrier.Set(zipkinTraceID, strconv.FormatUint(sc.TraceID, 16))
	carrier.Set(zipkinSpanID, strconv.FormatUint(sc.SpanID, 16))
	carrier.Set(zipkinSampled, strconv.FormatBool(sc.Sampled))

	if sc.ParentSpanID != nil {
		// we only set ParentSpanID header if there is a parent span
		carrier.Set(zipkinParentSpanID, strconv.FormatUint(*sc.ParentSpanID, 16))
	}
	// we only need to inject the debug flag if set. see flag package for details.
	flags := sc.Flags & flag.Debug
	carrier.Set(zipkinFlags, strconv.FormatUint(uint64(flags), 10))

	sc.baggageLock.Lock()
	for k, v := range sc.Baggage {
		carrier.Set(prefixBaggage+k, v)
	}
	sc.baggageLock.Unlock()
	return nil
}

func (p *textMapPropagator) Extract(
	opaqueCarrier interface{},
) (opentracing.SpanContext, error) {
	carrier, ok := opaqueCarrier.(opentracing.TextMapReader)
	if !ok {
		return nil, opentracing.ErrInvalidCarrier
	}
	requiredFieldCount := 0
	var (
		traceID      uint64
		spanID       uint64
		sampled      bool
		parentSpanID *uint64
		flags        flag.Flags
		err          error
	)
	decodedBaggage := make(map[string]string)
	err = carrier.ForeachKey(func(k, v string) error {
		switch strings.ToLower(k) {
		case zipkinTraceIDLower:
			traceID, err = strconv.ParseUint(v, 16, 64)
			if err != nil {
				return opentracing.ErrSpanContextCorrupted
			}
		case zipkinSpanIDLower:
			spanID, err = strconv.ParseUint(v, 16, 64)
			if err != nil {
				return opentracing.ErrSpanContextCorrupted
			}
		case zipkinParentSpanIDLower:
			var id uint64
			id, err = strconv.ParseUint(v, 16, 64)
			if err != nil {
				return opentracing.ErrSpanContextCorrupted
			}
			parentSpanID = &id
		case zipkinSampledLower:
			sampled, err = strconv.ParseBool(v)
			if err != nil {
				return opentracing.ErrSpanContextCorrupted
			}
			// Sampled header was explicitly set
			flags |= flag.SamplingSet
		case zipkinFlagsLower:
			var f uint64
			f, err = strconv.ParseUint(v, 10, 64)
			if err != nil {
				return opentracing.ErrSpanContextCorrupted
			}
			if flag.Flags(f)&flag.Debug == flag.Debug {
				flags |= flag.Debug
			}
		default:
			lowercaseK := strings.ToLower(k)
			if strings.HasPrefix(lowercaseK, prefixBaggage) {
				decodedBaggage[strings.TrimPrefix(lowercaseK, prefixBaggage)] = v
			}
			// Balance off the requiredFieldCount++ just below...
			requiredFieldCount--
		}
		requiredFieldCount++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if requiredFieldCount < tracerStateFieldCount {
		if requiredFieldCount == 0 {
			return nil, opentracing.ErrSpanContextNotFound
		}
		return nil, opentracing.ErrSpanContextCorrupted
	}

	// check if Sample state was communicated through the Flags bitset
	if !sampled && flags&flag.Sampled == flag.Sampled {
		sampled = true
	}

	return &SpanContext{
		TraceID:      traceID,
		SpanID:       spanID,
		Sampled:      sampled,
		Baggage:      decodedBaggage,
		ParentSpanID: parentSpanID,
		Flags:        flags,
	}, nil
}

func (p *binaryPropagator) Inject(
	spanContext opentracing.SpanContext,
	opaqueCarrier interface{},
) error {
	sc, ok := spanContext.(*SpanContext)
	if !ok {
		return opentracing.ErrInvalidSpanContext
	}
	carrier, ok := opaqueCarrier.(io.Writer)
	if !ok {
		return opentracing.ErrInvalidCarrier
	}

	state := wire.TracerState{}
	state.TraceId = sc.TraceID
	state.SpanId = sc.SpanID
	state.Sampled = sc.Sampled
	state.BaggageItems = sc.Baggage

	// encode the debug bit
	flags := sc.Flags & flag.Debug
	if sc.ParentSpanID != nil {
		state.ParentSpanId = *sc.ParentSpanID
	} else {
		// root span...
		state.ParentSpanId = 0
		flags |= flag.IsRoot
	}

	// we explicitly inform our sampling state downstream
	flags |= flag.SamplingSet
	if sc.Sampled {
		flags |= flag.Sampled
	}
	state.Flags = uint64(flags)

	b, err := proto.Marshal(&state)
	if err != nil {
		return err
	}

	// Write the length of the marshalled binary to the writer.
	length := uint32(len(b))
	if err = binary.Write(carrier, binary.BigEndian, &length); err != nil {
		return err
	}

	_, err = carrier.Write(b)
	return err
}

func (p *binaryPropagator) Extract(
	opaqueCarrier interface{},
) (opentracing.SpanContext, error) {
	carrier, ok := opaqueCarrier.(io.Reader)
	if !ok {
		return nil, opentracing.ErrInvalidCarrier
	}

	// Read the length of marshalled binary. io.ReadAll isn't that performant
	// since it keeps resizing the underlying buffer as it encounters more bytes
	// to read. By reading the length, we can allocate a fixed sized buf and read
	// the exact amount of bytes into it.
	var length uint32
	if err := binary.Read(carrier, binary.BigEndian, &length); err != nil {
		return nil, opentracing.ErrSpanContextCorrupted
	}
	buf := make([]byte, length)
	if n, err := carrier.Read(buf); err != nil {
		if n > 0 {
			return nil, opentracing.ErrSpanContextCorrupted
		}
		return nil, opentracing.ErrSpanContextNotFound
	}

	ctx := wire.TracerState{}
	if err := proto.Unmarshal(buf, &ctx); err != nil {
		return nil, opentracing.ErrSpanContextCorrupted
	}

	flags := flag.Flags(ctx.Flags)
	if flags&flag.Sampled == flag.Sampled {
		ctx.Sampled = true
	}
	// this propagator expects sampling state to be explicitly propagated by the
	// upstream service. so set this flag to indentify to tracer it should not
	// run its sampler in case it is not the root of the trace.
	flags |= flag.SamplingSet

	return &SpanContext{
		TraceID:      ctx.TraceId,
		SpanID:       ctx.SpanId,
		Sampled:      ctx.Sampled,
		Baggage:      ctx.BaggageItems,
		ParentSpanID: &ctx.ParentSpanId,
		Flags:        flags,
	}, nil
}
