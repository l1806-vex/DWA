package pow

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

type Solver struct {
	mu       sync.Mutex
	ctx      context.Context
	mod      api.Module
	stackFn  api.Function
	mallocFn api.Function
	solveFn  api.Function
}

func New(wasmPath string) (*Solver, error) {
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		return nil, fmt.Errorf("read wasm: %w", err)
	}

	r := wazero.NewRuntime(ctx)

	mod, err := r.InstantiateWithConfig(ctx, wasmBytes,
		wazero.NewModuleConfig().WithName("sha3_pow"),
	)
	if err != nil {
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}

	return &Solver{
		ctx:      ctx,
		mod:      mod,
		stackFn:  mod.ExportedFunction("__wbindgen_add_to_stack_pointer"),
		mallocFn: mod.ExportedFunction("__wbindgen_export_0"),
		solveFn:  mod.ExportedFunction("wasm_solve"),
	}, nil
}

// Solve returns the nonce satisfying DeepSeekHashV1, or an error.
func (s *Solver) Solve(challenge, salt string, difficulty, expireAt int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := fmt.Sprintf("%s_%d_", salt, expireAt)

	// Allocate 16-byte return slot on WASM stack.
	retRes, err := s.stackFn.Call(s.ctx, api.EncodeI32(-16))
	if err != nil {
		return 0, fmt.Errorf("stack alloc: %w", err)
	}
	ret := uint32(int32(retRes[0]))
	s.mod.Memory().Write(ret, make([]byte, 16))

	// Copy challenge string into WASM memory.
	chBytes := []byte(challenge)
	chPtrRes, err := s.mallocFn.Call(s.ctx, uint64(len(chBytes)), 1)
	if err != nil {
		return 0, fmt.Errorf("malloc challenge: %w", err)
	}
	chPtr := uint32(chPtrRes[0])
	s.mod.Memory().Write(chPtr, chBytes)

	// Copy prefix string into WASM memory.
	pfxBytes := []byte(prefix)
	pfxPtrRes, err := s.mallocFn.Call(s.ctx, uint64(len(pfxBytes)), 1)
	if err != nil {
		return 0, fmt.Errorf("malloc prefix: %w", err)
	}
	pfxPtr := uint32(pfxPtrRes[0])
	s.mod.Memory().Write(pfxPtr, pfxBytes)

	// Call wasm_solve.
	_, err = s.solveFn.Call(s.ctx,
		uint64(ret),
		uint64(chPtr), uint64(len(chBytes)),
		uint64(pfxPtr), uint64(len(pfxBytes)),
		math.Float64bits(float64(difficulty)),
	)
	if err != nil {
		return 0, fmt.Errorf("wasm_solve: %w", err)
	}

	// Read discriminant (i32 LE at ret+0).
	discRaw, _ := s.mod.Memory().Read(ret, 4)
	discriminant := int32(binary.LittleEndian.Uint32(discRaw))

	// Read answer (f64 LE at ret+8).
	ansRaw, _ := s.mod.Memory().Read(ret+8, 8)
	answer := math.Float64frombits(binary.LittleEndian.Uint64(ansRaw))

	// Restore stack.
	s.stackFn.Call(s.ctx, api.EncodeI32(16)) //nolint:errcheck

	if discriminant == 0 {
		return 0, fmt.Errorf("POW: no solution found within difficulty range")
	}
	return int64(answer), nil
}
