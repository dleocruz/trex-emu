// Copyright (c) 2020 Cisco Systems and/or its affiliates.
// Licensed under the Apache License, Version 2.0 (the "License");
// that can be found in the LICENSE file in the root of the source
// tree.

package netflow

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"unicode/utf8"
)

/* Field Engine offers the possibility to manipulate packets and parts of packets
by generating values in different forms */

// FieldEngineIF is a interface that every type of engine/generator should implement
// in order to provive common functionality to the caller. A caller doesn't care which
// type of field he is updating, but all the types must offer common functionality.
type FieldEngineIF interface {
	// Update updates the byte slice it receives with the new generated value.
	// It writes from the beginning of the slice.
	// If the length of the slice is shorter that the length of the variable we are
	// trying to write, it will return an error.
	// It is the responsibility of the caller to provide Update with a long enough
	// slice.
	Update(b []byte) error
	// GetOffset returns the offset of the packet as the interface was provived with.
	// The caller should use GetOffset to provide the interface with the correct
	// byte slice.
	GetOffset() uint16
	// GetSize() returns the size of the variable that the engine will write in
	// the slice byte the next time it will be called. In order to provide a slice
	// long enough, the caller should use GetSize.
	GetSize() uint16
}

// HistogramEntry is an interface for generic types of Histogram Engines.
// HistogramEngine is a fast non uniform pseudo random generator that can generate
// different types of elements. Each entry in this histogram must provide a probability
// for that entry to be chosen and a value for the histogram engine to output.
type HistogramEntry interface {
	// GetProb returns the probability of this entry to be chosen.
	GetProb() uint32
	// GetValue puts in the byte buffer the value that this entry outputs
	// in case it is picked. This value will be generated by the engine.
	// In case the input is incorrect and a value can't be generated, it will
	// return an error.
	GetValue() ([]byte, error)
}

/* ------------------------------------------------------------------------------
								UIntEngine
--------------------------------------------------------------------------------*/
// UIntEngine params is a struct a parameters for the UIntEngine.
type UIntEngineParams struct {
	size      uint16 // size of the uint variable in bytes
	offset    uint16 // offset in which to write in the packet
	op        string // operation which provides the generation, can be {inc, dec, rand}
	step      uint64 // step to decrement or increment, only in case these are the operations
	minValue  uint64 // minimal value of the domain
	maxValue  uint64 // maximal value of the domain
	initValue uint64 // initial value in the generator, if not provided it will be min value
}

// UIntEngine is a field engine which is responsible to generate variables of uint types.
// These types can be of lengths (1, 2, 4, 8) bytes (uint8, uint16, uint32, uint64).
// The next variable can be generated through different operations, a increment of the current value,
// a decrement of the current value, or some random generation.
type UIntEngine struct {
	par       *UIntEngineParams // params as provided by the caller
	currValue uint64            // current value in the generator
	domainLen uint64            // domain length
}

// maxUInt64 calculates the max between 2 uint64.
func maxUInt64(a, b uint64) (max uint64) {
	if a >= b {
		max = a
	} else {
		max = b
	}
	return max
}

// NewUintEngine creates a new uint engine
func NewUIntEngine(params *UIntEngineParams) (*UIntEngine, error) {
	o := new(UIntEngine)
	err := o.validateParams(params)
	if err != nil {
		return nil, err
	}
	o.par = params
	o.domainLen = (o.par.maxValue - o.par.minValue + 1)
	if o.domainLen == 0 {
		// when min = 0 and max = MaxUint64 domainLen can't be represented on a uint64.
		// So we try to get as close as we can.
		o.domainLen = math.MaxUint64
	}
	if o.par.step > o.domainLen {
		o.par.step = o.par.step % o.domainLen
	}
	o.currValue = maxUInt64(o.par.minValue, o.par.initValue)
	return o, nil
}

func findValue(arr []uint16, val uint16) (int, bool) {
	for i, item := range arr {
		if item == val {
			return i, true
		}
	}
	return -1, false
}

// ValidateParams validates the parameters
func (o *UIntEngine) validateParams(params *UIntEngineParams) (err error) {
	err = nil
	if params.minValue > params.maxValue {
		err = fmt.Errorf("Min value %v is bigger than max value %v.\n", params.minValue, params.maxValue)
	}
	if params.initValue != 0 && (params.initValue < params.minValue || params.initValue > params.maxValue) {
		err = fmt.Errorf("Init value %v must be between [%v - %v].\n", params.initValue, params.minValue, params.maxValue)
	}
	sizes := []uint16{1, 2, 4, 8}
	maxPossible := []uint64{math.MaxUint8, math.MaxUint16, math.MaxUint32, math.MaxUint64}
	i, ok := findValue(sizes, params.size)
	if !ok {
		err = fmt.Errorf("Invalid size %v. Size should be {1, 2, 4, 8}.\n", params.size)
	} else {
		if params.maxValue > maxPossible[i] {
			err = fmt.Errorf("Max value %v cannot be represented with size %v.\n", params.maxValue, params.size)
		}
	}
	if params.op != "inc" && params.op != "dec" && params.op != "rand" {
		err = fmt.Errorf("Unsupported operation %v.\n", params.op)
	}
	return err
}

// IncValue increments the value according to step
func (o *UIntEngine) IncValue() {
	// Need to be very careful here with overflows.
	left := o.par.maxValue - o.currValue // this will never overflow as currValue < maxValue
	if o.par.step <= left {
		// simple increment by step, not overflow of domain
		// step is fixed module size of domain
		o.currValue += o.par.step
	} else {
		// overflow of domain
		// if here then (step > left) therefore step - left - 1 will not overflow
		o.currValue = o.par.minValue + (o.par.step - left - 1) // restart also consumes 1
	}
}

// DecValue decrements the value according to step
func (o *UIntEngine) DecValue() {
	left := o.currValue - o.par.minValue // this will never overflow as currValue > minValue
	if o.par.step <= left {
		// no overflow of domain
		// step is fixed module size of domain
		o.currValue -= o.par.step
	} else {
		// overflow of domain
		// if here then (step > left) therefore step - left - 1 will not overflow
		o.currValue = o.par.maxValue - (o.par.step - left - 1) // restart also consumes 1
	}
}

// RandValue generates a random value in the domain [min, max]
func (o *UIntEngine) RandValue() {
	// Generates a uint64 with uniform distribution.
	// Converts the generated value to a value in the domain by adding the modulus of domainLength
	// to the minimal value.
	genValue := rand.Uint64()
	o.currValue = o.par.minValue + (genValue % o.domainLen)
}

// PerformOp performs the operation, either it is rand, inc or dec.
func (o *UIntEngine) PerformOp() (err error) {
	err = nil
	switch o.par.op {
	case "inc":
		o.IncValue()
	case "dec":
		o.DecValue()
	case "rand":
		o.RandValue()
	default:
		err = errors.New("Unrecognized operation")
	}
	return err
}

// Update implements the Update function of FieldEngineIF.
func (o *UIntEngine) Update(b []byte) error {
	if len(b) < int(o.par.size) {
		return fmt.Errorf("Provided slice is shorter that the size of the variable to write, want at least %v, have %v.\n", o.par.size, len(b))
	}
	switch o.par.size {
	case 1:
		b[0] = uint8(o.currValue)
	case 2:
		binary.BigEndian.PutUint16(b, uint16(o.currValue))
	case 4:
		binary.BigEndian.PutUint32(b, uint32(o.currValue))
	case 8:
		binary.BigEndian.PutUint64(b, o.currValue)
	default:
		return errors.New("Size should be 1, 2, 4 or 8.")
	}
	err := o.PerformOp()
	if err != nil {
		return err
	}
	return nil
}

// GetOffset implements the GetOffset function of FieldEngineIF.
func (o *UIntEngine) GetOffset() uint16 {
	return o.par.offset
}

// GetSize implements the GetSize function of FieldEngineIF.
func (o *UIntEngine) GetSize() uint16 {
	return o.par.size
}

/* ------------------------------------------------------------------------------
							HistogramEngine
--------------------------------------------------------------------------------*/
// HistogramEngineParams is a struct that must be provided to the HistogramEngine
// when creating it.
type HistogramEngineParams struct {
	size    uint16           // size of the uint variable in bytes
	offset  uint16           // offset in which to write in the packet
	entries []HistogramEntry // the entries of the histogram
}

// HistogramEngine is a FieldEngine, which contains a non uniform pseudo random
// generator and can generate it's entries as per probability of each entry.
type HistogramEngine struct {
	par           *HistogramEngineParams // params as provided by the caller
	distributions []uint32               // distribution slice
	generator     *NonUniformRandGen     // non uniform random generator per distribution

}

// NewHistogramEngine creates a new HistogramEngine from the HistogramEngineParams provided.
func NewHistogramEngine(params *HistogramEngineParams) (o *HistogramEngine, err error) {
	o = new(HistogramEngine)
	o.buildDistributionSlice(params.entries)
	o.par = params
	o.generator, err = NewNonUniformRandGen(o.distributions)
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (o *HistogramEngine) buildDistributionSlice(entries []HistogramEntry) {
	for _, entry := range entries {
		o.distributions = append(o.distributions, entry.GetProb())
	}
}

// Update implements the Update function of FieldEngineIF.
func (o *HistogramEngine) Update(b []byte) error {
	if len(b) < int(o.par.size) {
		return fmt.Errorf("Provided slice is shorter that the size of the variable to write, want at least %v, have %v.\n", o.par.size, len(b))
	}
	entryIndex := o.generator.Generate()
	entry := o.par.entries[entryIndex]
	newValueBytes, err := entry.GetValue()
	if err != nil {
		return err
	}
	if len(newValueBytes) < int(o.par.size) {
		return fmt.Errorf("New value length is shorter that it should be, want %v, have %v.\n", o.par.size, len(newValueBytes))
	}
	copiedSize := copy(b[:o.par.size], newValueBytes[:o.par.size])
	if copiedSize != int(o.par.size) {
		return fmt.Errorf("Didn't copy the right amount to the buffer, want %v have %v.\n", o.par.size, copiedSize)
	}
	return nil
}

// GetOffset implements the GetOffset function of FieldEngineIF.
func (o *HistogramEngine) GetOffset() uint16 {
	return o.par.offset
}

// GetSize implements the GetSize function of FieldEngineIF.
func (o *HistogramEngine) GetSize() uint16 {
	return o.par.size
}

/* ------------------------------------------------------------------------------
						HistogramUInt32Entry
--------------------------------------------------------------------------------*/
// HistogramUInt32Entry represents a uint32 which can be used as an entry for the
// HistogramEngine. This entry can be picked with probability prob.
type HistogramUInt32Entry struct {
	v    uint32 // a value v of 32 bits
	prob uint32 // probability of this entry
}

// GetValue puts the value on the byte buffer.
func (o *HistogramUInt32Entry) GetValue() (b []byte, err error) {
	b = make([]byte, 4)
	binary.BigEndian.PutUint32(b, o.v)
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramUInt32Entry) GetProb() uint32 {
	return o.prob
}

/* ------------------------------------------------------------------------------
						HistogramUInt32RangeEntry
--------------------------------------------------------------------------------*/
// HistogramUInt32RangeEntry represents a range of uint32 which can be used as an
// entry for the HistogramEngine. This entry can be picked with probability prob.
// If the entry is picked, a value in the range will be generated uniformly.
type HistogramUInt32RangeEntry struct {
	min  uint32 // lower bound of the range
	max  uint32 // higher bound of the range
	prob uint32 // probability of this entry
}

// GetValue generates uniformly a value in the range and puts it on the byte buffer.
func (o *HistogramUInt32RangeEntry) GetValue() (b []byte, err error) {
	if o.max < o.min {
		return nil, fmt.Errorf("Max %v is smaller than min %v in HistogramRuneRangeEntry.\n", o.max, o.min)
	}
	b = make([]byte, 4)
	v := rand.Uint32()                    // generate random 32 bytes
	v = o.min + (v % (o.max - o.min + 1)) // scale it on the domain
	binary.BigEndian.PutUint32(b, v)      // put it on the bytes buffer
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramUInt32RangeEntry) GetProb() uint32 {
	return o.prob
}

/* ------------------------------------------------------------------------------
						HistogramUInt32ListEntry
--------------------------------------------------------------------------------*/
// HistogramUInt32ListEntry represents a list of uint32 which can be used as an
// entry for the HistogramEngine. This entry can be picked with probability prob.
// If the entry is picked, a value in the list will be selected uniformly.
type HistogramUInt32ListEntry struct {
	list []uint32 // a list from where the element will be picked
	prob uint32   // probability of this entry
}

// GetValue picks a random value from the list and puts it on the byte buffer.
func (o *HistogramUInt32ListEntry) GetValue() (b []byte, err error) {
	if o.list == nil || len(o.list) == 0 {
		return nil, fmt.Errorf("Empty list in HistogramUInt32ListEntry.\n")
	}
	b = make([]byte, 4)
	index := rand.Intn(len(o.list))
	binary.BigEndian.PutUint32(b, o.list[index])
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramUInt32ListEntry) GetProb() uint32 {
	return o.prob
}

/* ------------------------------------------------------------------------------
						HistogramRuneEntry
--------------------------------------------------------------------------------*/
// HistogramRuneEntry represents a rune which can be used as an entry for the
// HistogramEngine. This entry can be picked with probability prob.
type HistogramRuneEntry struct {
	r    rune   // value of the rune
	prob uint32 // probability of this entry
}

// GetValue puts the rune value in the bytes buffer.
func (o *HistogramRuneEntry) GetValue() (b []byte, err error) {
	b = make([]byte, utf8.RuneLen(o.r))
	utf8.EncodeRune(b, o.r)
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramRuneEntry) GetProb() uint32 {
	return o.prob
}

/* ------------------------------------------------------------------------------
						HistogramRuneRange
--------------------------------------------------------------------------------*/
// HistogramRuneRange represents a range of runes which can be used as an
// entry for the HistogramEngine. This entry can be picked with probability prob.
// If the entry is picked, a rune in the range will be generated uniformly.
type HistogramRuneRangeEntry struct {
	min  rune   // min value of the rune
	max  rune   // max value of the rune
	prob uint32 // probability of this entry
}

// GetValue generates uniformly a rune in the range and puts it on the byte buffer.
// This buffer is going to be passed to the engine, so make sure that the engine
// buffer is at least the same size as this buffer.
func (o *HistogramRuneRangeEntry) GetValue() (b []byte, err error) {
	if o.max < o.min {
		return nil, fmt.Errorf("Max %v is smaller than min %v in HistogramRuneRangeEntry.\n", o.max, o.min)
	}
	r := o.min + rune(rand.Intn(int(o.max-o.min+1)))
	b = make([]byte, utf8.RuneLen(r))
	utf8.EncodeRune(b, r)
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramRuneRangeEntry) GetProb() uint32 {
	return o.prob
}

/* ------------------------------------------------------------------------------
						HistogramRuneList
--------------------------------------------------------------------------------*/
// HistogramRuneListEntry represents a list of runes which can be used as an
// entry for the HistogramEngine. This entry can be picked with probability prob.
// If the entry is picked, a rune in the list will be selected uniformly.
type HistogramRuneListEntry struct {
	list []rune // min value of the rune
	prob uint32 // probability of this entry
}

// GetValue puts the selected rune value in the bytes buffer.
// This buffer is going to be passed to the engine, so make sure that the engine
// buffer is at least the same size as this buffer.
func (o *HistogramRuneListEntry) GetValue() (b []byte, err error) {
	if o.list == nil || len(o.list) == 0 {
		return nil, fmt.Errorf("Empty list in HistogramRuneListEntry.\n")
	}
	r := o.list[rand.Intn(len(o.list))]
	b = make([]byte, utf8.RuneLen(r))
	utf8.EncodeRune(b, r)
	return b, nil
}

// GetProb returns the probability for this entry to be picked in the histogram engine.
func (o *HistogramRuneListEntry) GetProb() uint32 {
	return o.prob
}