package actors_test

import (
	"context"
	"testing"

	"github.com/ipfs/go-cid"
	dstore "github.com/ipfs/go-datastore"
	hamt "github.com/ipfs/go-hamt-ipld"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	cbor "github.com/ipfs/go-ipld-cbor"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-lotus/chain/actors"
	"github.com/filecoin-project/go-lotus/chain/address"
	"github.com/filecoin-project/go-lotus/chain/gen"
	"github.com/filecoin-project/go-lotus/chain/state"
	"github.com/filecoin-project/go-lotus/chain/store"
	"github.com/filecoin-project/go-lotus/chain/types"
	"github.com/filecoin-project/go-lotus/chain/vm"
	"github.com/filecoin-project/go-lotus/chain/wallet"
)

type HarnessInit struct {
	NAddrs uint64
	Addrs  map[address.Address]types.BigInt
	Miner  address.Address
}

type HarnessStage int

const (
	HarnessPreInit HarnessStage = iota
	HarnessPostInit
)

type HarnessOpt func(testing.TB, *Harness2) error

type Harness2 struct {
	HI     HarnessInit
	Stage  HarnessStage
	Nonces map[address.Address]uint64

	ctx context.Context
	bs  blockstore.Blockstore
	vm  *vm.VM
	cs  *store.ChainStore
	w   *wallet.Wallet
}

var HarnessMinerFunds = types.NewInt(1000000)

func HarnessAddr(addr *address.Address, value uint64) HarnessOpt {
	return func(t testing.TB, h *Harness2) error {
		if h.Stage != HarnessPreInit {
			return nil
		}
		hi := &h.HI
		if addr.Empty() {
			k, err := h.w.GenerateKey(types.KTSecp256k1)
			if err != nil {
				t.Fatal(err)
			}

			*addr = k
		}
		hi.Addrs[*addr] = types.NewInt(value)
		return nil
	}
}

func HarnessMiner(addr *address.Address) HarnessOpt {
	return func(_ testing.TB, h *Harness2) error {
		if h.Stage != HarnessPreInit {
			return nil
		}
		hi := &h.HI
		if addr.Empty() {
			*addr = hi.Miner
			return nil
		}
		delete(hi.Addrs, hi.Miner)
		hi.Miner = *addr
		return nil
	}
}

func HarnessActor(actor *address.Address, creator *address.Address, code cid.Cid, params func() interface{}) HarnessOpt {
	return func(t testing.TB, h *Harness2) error {
		if h.Stage != HarnessPostInit {
			return nil
		}
		if !actor.Empty() {
			return xerrors.New("actor address should be empty")
		}

		ret, _ := h.CreateActor(t, *creator, code, params())
		if ret.ExitCode != 0 {
			return xerrors.Errorf("creating actor: %w", ret.ActorErr)
		}
		var err error
		*actor, err = address.NewFromBytes(ret.Return)
		return err
	}

}

func HarnessCtx(ctx context.Context) HarnessOpt {
	return func(t testing.TB, h *Harness2) error {
		h.ctx = ctx
		return nil
	}
}

func NewHarness2(t *testing.T, options ...HarnessOpt) *Harness2 {
	w, err := wallet.NewWallet(wallet.NewMemKeyStore())
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness2{
		Stage:  HarnessPreInit,
		Nonces: make(map[address.Address]uint64),
		HI: HarnessInit{
			NAddrs: 1,
			Miner:  blsaddr(0),
			Addrs: map[address.Address]types.BigInt{
				blsaddr(0): HarnessMinerFunds,
			},
		},

		w:   w,
		ctx: context.Background(),
		bs:  bstore.NewBlockstore(dstore.NewMapDatastore()),
	}
	for _, opt := range options {
		err := opt(t, h)
		if err != nil {
			t.Fatalf("Applying options: %v", err)
		}
	}

	st, err := gen.MakeInitialStateTree(h.bs, h.HI.Addrs)
	if err != nil {
		t.Fatal(err)
	}

	stateroot, err := st.Flush()
	if err != nil {
		t.Fatal(err)
	}
	h.cs = store.NewChainStore(h.bs, nil)
	h.vm, err = vm.NewVM(stateroot, 1, h.HI.Miner, h.cs)
	if err != nil {
		t.Fatal(err)
	}
	h.Stage = HarnessPostInit
	for _, opt := range options {
		err := opt(t, h)
		if err != nil {
			t.Fatalf("Applying options: %v", err)
		}
	}

	return h
}

func (h *Harness2) Apply(t testing.TB, msg types.Message) (*vm.ApplyRet, *state.StateTree) {
	t.Helper()
	if msg.Nonce == 0 {
		msg.Nonce, _ = h.Nonces[msg.From]
		h.Nonces[msg.From] = msg.Nonce + 1
	}

	ret, err := h.vm.ApplyMessage(h.ctx, &msg)
	if err != nil {
		t.Fatalf("Applying message: %+v", err)
	}
	stateroot, err := h.vm.Flush(context.TODO())
	if err != nil {
		t.Fatalf("Flushing VM: %+v", err)
	}
	cst := hamt.CSTFromBstore(h.bs)
	state, err := state.LoadStateTree(cst, stateroot)
	if err != nil {
		t.Fatalf("Loading state tree: %+v", err)
	}
	return ret, state
}

func (h *Harness2) CreateActor(t testing.TB, from address.Address,
	code cid.Cid, params interface{}) (*vm.ApplyRet, *state.StateTree) {
	t.Helper()

	return h.Apply(t, types.Message{
		To:     actors.InitActorAddress,
		From:   from,
		Method: actors.IAMethods.Exec,
		Params: DumpObject(t,
			&actors.ExecParams{
				Code:   code,
				Params: DumpObject(t, params),
			}),
		GasPrice: types.NewInt(1),
		GasLimit: types.NewInt(1),
		Value:    types.NewInt(0),
	})
}

func (h *Harness2) SendFunds(t testing.TB, from address.Address, to address.Address,
	value types.BigInt) (*vm.ApplyRet, *state.StateTree) {
	t.Helper()
	return h.Apply(t, types.Message{
		To:       to,
		From:     from,
		Method:   0,
		Value:    value,
		GasPrice: types.NewInt(1),
		GasLimit: types.NewInt(1),
	})
}

func (h *Harness2) Invoke(t testing.TB, from address.Address, to address.Address,
	method uint64, params interface{}) (*vm.ApplyRet, *state.StateTree) {
	t.Helper()
	return h.Apply(t, types.Message{
		To:       to,
		From:     from,
		Method:   method,
		Value:    types.NewInt(0),
		Params:   DumpObject(t, params),
		GasPrice: types.NewInt(1),
		GasLimit: types.NewInt(1),
	})
}

func (h *Harness2) AssertBalance(t testing.TB, addr address.Address, amt uint64) {
	t.Helper()

	b, err := h.vm.ActorBalance(addr)
	if err != nil {
		t.Fatal(err)
	}

	if types.BigCmp(types.NewInt(amt), b) != 0 {
		t.Fatalf("expected %s to have balanced of %d. Instead has %s", addr, amt, b)
	}
}

func DumpObject(t testing.TB, obj interface{}) []byte {
	t.Helper()
	enc, err := cbor.DumpObject(obj)
	if err != nil {
		t.Fatalf("dumping params: %+v", err)
	}
	return enc
}