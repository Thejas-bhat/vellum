//  Copyright (c) 2017 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vellum

import (
	"encoding/binary"
	"fmt"
	"io"
)

const versionV1 = 1
const oneTransition = 1 << 7
const transitionNext = 1 << 6
const stateFinal = 1 << 6
const footerSizeV1 = 16

func init() {
	registerEncoder(versionV1, func(w io.Writer) encoder {
		return newEncoderV1(w)
	})
}

type encoderV1 struct {
	bw         *writer
	outputType int
}

func newEncoderV1(w io.Writer) *encoderV1 {
	return &encoderV1{
		bw: newWriter(w),
	}
}

func (e *encoderV1) setOutputType(typ int) {
	e.outputType = typ
}

func (e *encoderV1) reset(w io.Writer) {
	e.bw.Reset(w)
}

func (e *encoderV1) start() error {
	header := make([]byte, headerSize)
	binary.LittleEndian.PutUint64(header, versionV1)
	if e.outputType&storeIntSlice > 0 {
		binary.LittleEndian.PutUint64(header[8:], uint64(storeIntSlice)) // type
	} else {
		binary.LittleEndian.PutUint64(header[8:], uint64(0))
	}

	n, err := e.bw.Write(header)
	if err != nil {
		return err
	}
	if n != headerSize {
		return fmt.Errorf("short write of header %d/%d", n, headerSize)
	}
	return nil
}

func (e *encoderV1) isEmptyFinalOutput(s *builderNode) bool {
	switch e.outputType {
	case storeIntSlice:
		val, _ := s.finalOutput.([]uint64)
		return len(val) == 0
	default:
		val, _ := s.finalOutput.(uint64)
		return val == 0
	}
}

func (e *encoderV1) encodeState(s *builderNode, lastAddr int) (int, error) {
	if len(s.trans) == 0 && s.final && e.isEmptyFinalOutput(s) {
		return 0, nil
	} else if len(s.trans) != 1 || s.final {
		return e.encodeStateMany(s)
	} else if !s.final && e.isTransOutEmpty(&s.trans[0]) && s.trans[0].addr == lastAddr {
		return e.encodeStateOneFinish(s, transitionNext)
	}
	return e.encodeStateOne(s)
}

func (e *encoderV1) encodeStateOne(s *builderNode) (int, error) {
	start := uint64(e.bw.counter)
	outPackSize := 0
	if !e.isTransOutEmpty(&s.trans[0]) {
		outPackSize = packedSize(s.trans[0].out)
		err := e.bw.WritePackedOutput(s.trans[0].out, outPackSize)
		if err != nil {
			return 0, err
		}
	}
	delta := deltaAddr(start, uint64(s.trans[0].addr))
	transPackSize := packedSize(delta)
	err := e.bw.WritePackedUintIn(delta, transPackSize)
	if err != nil {
		return 0, err
	}

	packSize := encodePackSize(transPackSize, outPackSize)
	err = e.bw.WriteByte(packSize)
	if err != nil {
		return 0, err
	}

	return e.encodeStateOneFinish(s, 0)
}

func (e *encoderV1) encodeStateOneFinish(s *builderNode, next byte) (int, error) {
	enc := encodeCommon(s.trans[0].in)

	// not a common input
	if enc == 0 {
		err := e.bw.WriteByte(s.trans[0].in)
		if err != nil {
			return 0, err
		}
	}
	err := e.bw.WriteByte(oneTransition | next | enc)
	if err != nil {
		return 0, err
	}

	return e.bw.counter - 1, nil
}

func (e *encoderV1) isTransOutEmpty(t *transition) bool {
	switch e.outputType {
	case storeIntSlice:
		val, _ := t.out.([]uint64)
		return len(val) == 0
	default:
		val, _ := t.out.(uint64)
		return val == 0
	}
}

// we could have something similar to how deltas are stored
// to store the outsizes.

// and then store the totalOutSizes as part of the packSize
// below. totalOutSizes = sum of packSizes?

// So format could look something like:
//
//							1 byte								    numTrans      numTrans * transPackSizes    numTrans * outSizesPackSize	        sum(outSizes[i])
//	   <packSize(transPackSize/outSizesPackSize)>             <<trans keys>>        <<deltas>>                  <<!outSizes!>>                       <<outputs>>
func (e *encoderV1) encodeStateMany(s *builderNode) (int, error) {
	start := uint64(e.bw.counter)
	transPackSize := 0
	var outSizes []uint64
	outPackSize := uint64(packedSize(s.finalOutput))
	outSizesPackSize := packedSize(outPackSize)
	anyOutputs := !e.isEmptyFinalOutput(s)

	// We find the max transPackSize and the outPackSize
	// using which we encode each transition's output.
	// But when it comes storing int slice, we would
	// end up padding all the empty spaces and cause
	// redundant disk use
	for i := range s.trans {
		// having fixed transSize is fine I think?
		delta := deltaAddr(start, uint64(s.trans[i].addr))
		tsize := packedSize(delta)
		if tsize > transPackSize {
			transPackSize = tsize
		}
		// this could different per out to save space.
		// so maintain an array of it.
		// however we can pack a max size within this
		// array of outSizes
		osize := uint64(packedSize(s.trans[i].out))
		outSizes = append(outSizes, osize)
		packosize := packedSize(osize)
		if packosize > outSizesPackSize {
			outSizesPackSize = packosize
		}
		anyOutputs = anyOutputs || !e.isTransOutEmpty(&s.trans[i])
	}

	if s.final {
		outSizes = append(outSizes, outPackSize)
	}

	if !anyOutputs {
		outSizesPackSize = 0
	}

	if anyOutputs {
		// output final value
		if s.final {
			err := e.bw.WritePackedOutput(s.finalOutput, int(outSizes[len(outSizes)-1]))
			if err != nil {
				return 0, err
			}
		}
		// output transition values (in reverse)
		for j := len(s.trans) - 1; j >= 0; j-- {
			packSize := int(outSizes[j])
			// could end up being something like outSize[i] or something?
			err := e.bw.WritePackedOutput(s.trans[j].out, packSize)
			if err != nil {
				return 0, err
			}
		}
	}

	// write the outSizes, corresponding to each output
	if e.outputType == storeIntSlice {
		for j := len(outSizes) - 1; j >= 0; j-- {
			err := e.bw.WritePackedUintIn(outSizes[j], outSizesPackSize)
			if err != nil {
				return 0, err
			}
		}
	}

	// output transition dests (in reverse)
	for j := len(s.trans) - 1; j >= 0; j-- {
		delta := deltaAddr(start, uint64(s.trans[j].addr))
		err := e.bw.WritePackedUintIn(delta, transPackSize)
		if err != nil {
			return 0, err
		}
	}

	// output transition keys (in reverse)
	for j := len(s.trans) - 1; j >= 0; j-- {
		err := e.bw.WriteByte(s.trans[j].in)
		if err != nil {
			return 0, err
		}
	}

	packSize := encodePackSize(transPackSize, outSizesPackSize)
	err := e.bw.WriteByte(packSize)
	if err != nil {
		return 0, err
	}

	numTrans := encodeNumTrans(len(s.trans))
	// if number of transitions wont fit in edge header byte
	// write out separately
	if numTrans == 0 {
		if len(s.trans) == 256 {
			// this wouldn't fit in single byte, but reuse value 1
			// which would have always fit in the edge header instead
			err = e.bw.WriteByte(1)
			if err != nil {
				return 0, err
			}
		} else {
			err = e.bw.WriteByte(byte(len(s.trans)))
			if err != nil {
				return 0, err
			}
		}
	}

	// finally write edge header
	if s.final {
		numTrans |= stateFinal
	}
	err = e.bw.WriteByte(numTrans)
	if err != nil {
		return 0, err
	}

	return e.bw.counter - 1, nil
}

func (e *encoderV1) finish(count, rootAddr int) error {
	footer := make([]byte, footerSizeV1)
	binary.LittleEndian.PutUint64(footer, uint64(count))        // root addr
	binary.LittleEndian.PutUint64(footer[8:], uint64(rootAddr)) // root addr
	n, err := e.bw.Write(footer)
	if err != nil {
		return err
	}
	if n != footerSizeV1 {
		return fmt.Errorf("short write of footer %d/%d", n, footerSizeV1)
	}
	err = e.bw.Flush()
	if err != nil {
		return err
	}
	return nil
}
