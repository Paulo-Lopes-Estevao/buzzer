// Copyright 2023 Google LLC
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

// Package parseverifier implements a strategy of generating random
// ALU operations and then attempting to hunt verifier logic errors by parsing
// the output of the vierifier log and comparing the values the verifier thinks
// the registers will have vs the actual values that are observed at run time.
package parseverifier

//#include <stdlib.h>
//void close_fd(int fd);
import "C"

import (
	"errors"
	"fmt"

	fpb "buzzer/proto/ebpf_fuzzer_go_proto"
	"buzzer/pkg/ebpf/ebpf"
	"buzzer/pkg/strategies/parse_verifier/oracle/oracle"
	"buzzer/pkg/strategies/strategies"
)

const (
	// StrategyName exposes the value of the flag that should be used to
	// invoke this strategy.
	StrategyName = "parse_verifier_log"
)

// StrategyParseVerifierLog Implements a fuzzing strategy where the results of
// the ebpf verifier will be parsed and then compared with the actual values
// observed at run time.
type StrategyParseVerifierLog struct{}

func (st *StrategyParseVerifierLog) generateAndValidateProgram(e strategies.ExecutorInterface, gen *Generator) (*strategies.GeneratorResult, error) {
	for i := 0; i < 100_000; i++ {
		prog, err := ebpf.New(gen /*mapSize=*/, 1000 /*minReg=*/, ebpf.RegR7 /*maxReg=*/, ebpf.RegR9)
		if err != nil {
			return nil, err
		}
		byteCode := prog.GenerateBytecode()
		res, err := e.ValidateProgram(byteCode)
		if err != nil {
			prog.Cleanup()
			return nil, err
		}

		if res.GetIsValid() {
			result := &strategies.GeneratorResult{
				Prog:         prog,
				ProgByteCode: byteCode,
				ProgFD:       res.GetProgramFd(),
				VerifierLog:  res.GetVerifierLog(),
			}

			return result, nil
		}
		prog.Cleanup()
	}
	return nil, errors.New("could not generate a valid program")
}

// Fuzz implements the main fuzzing logic.
func (st *StrategyParseVerifierLog) Fuzz(e strategies.ExecutorInterface) error {
	fmt.Printf("running fuzzing strategy %s\n", StrategyName)
	i := 0
	for {
		gen := &Generator{
			instructionCount: 10,
			offsetMap:        make(map[int32]int32),
			sizeMap:          make(map[int32]int32),
			regMap:           make(map[int32]uint8),
		}
		fmt.Printf("Fuzzer run no %d.                               \r", i)
		i++
		gr, err := st.generateAndValidateProgram(e, gen)

		if err != nil {
			return err
		}

		// Build a new execution request.
		logMap := gr.Prog.LogMap()
		logCount := gen.logCount
		rpr := &fpb.RunProgramRequest {
			ProgFd:      gr.ProgFD,
			MapFd:       int64(logMap),
			MapCount:    logCount,
			EbpfProgram: gr.ProgByteCode,
		}

		defer func() {
			C.close_fd(C.int(rpr.GetProgFd()))
			C.close_fd(C.int(rpr.GetMapFd()))
		}()

		programFlaked := true

		var exRes *fpb.ExecutionResult
		maxAttempts := 1000

		for programFlaked && maxAttempts != 0 {
			maxAttempts--
			eR, err := e.RunProgram(rpr)
			if err != nil {
				return err
			}

			if !eR.GetDidSucceed() {
				return fmt.Errorf("execute Program did not succeed")
			}
			for i := 0; i < len(eR.GetElements()); i++ {
				if eR.GetElements()[i] != 0 {
					programFlaked = false
					exRes = eR
					break
				}
			}
		}

		if maxAttempts == 0 {
			fmt.Println("program flaked")
			strategies.SaveExecutionResults(gr)
			continue
		}

		// Program succeeded, let's validate the execution map.
		regOracle, err := oracle.FromVerifierTrace(gr.VerifierLog)
		if err != nil {
			return err
		}

		for mapIndex := int32(0); mapIndex < rpr.GetMapCount(); mapIndex++ {
			offset := gen.GetProgramOffset(mapIndex)
			dstReg := gen.GetDestReg(mapIndex)
			verifierValue, known, err := regOracle.LookupRegValue(offset, dstReg)
			if err != nil {
				return err
			}
			actualValue := exRes.GetElements()[mapIndex]
			if known && verifierValue != actualValue {
				if err := strategies.SaveExecutionResults(gr); err != nil {
					return err
				}
			}
		}

		C.close_fd(C.int(rpr.GetProgFd()))
		C.close_fd(C.int(rpr.GetMapFd()))
	}
	return nil
}
