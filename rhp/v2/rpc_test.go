package rhp_test

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"go.sia.tech/hostd/internal/merkle"
	"go.sia.tech/hostd/internal/test"
	"go.sia.tech/hostd/rhp/v2"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/types"
	"lukechampine.com/frand"
)

func TestSettings(t *testing.T) {
	renter, host, err := test.NewTestingPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer renter.Close()
	defer host.Close()

	hostSettings, err := host.RHPv2Settings()
	if err != nil {
		t.Fatal(err)
	}

	renterSettings, err := renter.Settings(context.Background(), host.RHPv2Addr(), host.PublicKey())
	if err != nil {
		t.Fatal(err)
	}

	// note: cannot use reflect.DeepEqual directly because the types are different
	hostVal := reflect.ValueOf(hostSettings)
	renterVal := reflect.ValueOf(renterSettings)
	if hostVal.NumField() != renterVal.NumField() {
		t.Fatalf("mismatched number of fields: host %v, renter %v", hostVal.NumField(), renterVal.NumField())
	}

	for i := 0; i < hostVal.NumField(); i++ {
		fieldName := hostVal.Type().Field(i).Name
		hostField := hostVal.FieldByName(fieldName)
		renterField := renterVal.FieldByName(fieldName)

		// check if the types are equal
		if hostField.Kind() != renterField.Kind() {
			t.Fatalf("field %s mismatch: host %v, renter %v", fieldName, hostField.Kind(), renterField.Kind())
		}

		// get the underlying values
		va := hostField.Interface()
		vb := renterField.Interface()

		if !reflect.DeepEqual(va, vb) {
			t.Errorf("field %s mismatch: host %v, renter %v", fieldName, hostField.Interface(), renterField.Interface())
		}
	}
}

func TestUploadDownload(t *testing.T) {
	renter, host, err := test.NewTestingPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer renter.Close()
	defer host.Close()

	// form a contract
	contract, err := renter.FormContract(context.Background(), host.RHPv2Addr(), host.PublicKey(), types.SiacoinPrecision.Mul64(10), types.SiacoinPrecision.Mul64(20), 200)
	if err != nil {
		t.Fatal(err)
	}

	// mine a block to confirm the contract
	if err := host.MineBlocks(host.WalletAddress(), 1); err != nil {
		t.Fatal(err)
	}

	session, err := renter.NewRHP2Session(context.Background(), host.RHPv2Addr(), host.PublicKey(), contract.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// generate a sector
	sector := make([]byte, rhp.SectorSize)
	frand.Read(sector[:256])
	sectorRoot := merkle.SectorRoot(sector)

	// calculate the remaining duration of the contract
	var remainingDuration uint64
	contractExpiration := uint64(session.Revision().NewWindowEnd)
	currentHeight := renter.TipState().Index.Height
	if contractExpiration < currentHeight {
		t.Fatal("contract expired")
	}
	// upload the sector
	remainingDuration = contractExpiration - currentHeight
	if writtenRoot, err := session.Append(context.Background(), sector, remainingDuration); err != nil {
		t.Fatal(err)
	} else if writtenRoot != sectorRoot {
		t.Fatal("sector root mismatch")
	}

	// check the host's sector roots matches the sector we just uploaded
	roots, err := session.SectorRoots(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	} else if roots[0] != sectorRoot {
		t.Fatal("sector root mismatch")
	}

	// check that the revision fields are correct
	revision := session.Revision()
	switch {
	case revision.NewFileSize != rhp.SectorSize:
		t.Fatal("wrong filesize")
	case revision.NewFileMerkleRoot != sectorRoot:
		t.Fatal("wrong merkle root")
	}
}

func BenchmarkUpload(b *testing.B) {
	renter, host, err := test.NewTestingPair(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer renter.Close()
	defer host.Close()

	if err := host.AddVolume(filepath.Join(b.TempDir(), "storage.dat"), uint64(b.N)); err != nil {
		b.Fatal(err)
	}

	// form a contract
	contract, err := renter.FormContract(context.Background(), host.RHPv2Addr(), host.PublicKey(), types.SiacoinPrecision.Mul64(10), types.SiacoinPrecision.Mul64(20), 200)
	if err != nil {
		b.Fatal(err)
	}

	// mine a block to confirm the contract
	if err := host.MineBlocks(host.WalletAddress(), 1); err != nil {
		b.Fatal(err)
	}

	session, err := renter.NewRHP2Session(context.Background(), host.RHPv2Addr(), host.PublicKey(), contract.ID())
	if err != nil {
		b.Fatal(err)
	}
	defer session.Close()

	// calculate the remaining duration of the contract
	var remainingDuration uint64
	contractExpiration := uint64(session.Revision().NewWindowEnd)
	currentHeight := renter.TipState().Index.Height
	if contractExpiration < currentHeight {
		b.Fatal("contract expired")
	}
	remainingDuration = contractExpiration - currentHeight

	b.ReportAllocs()
	b.SetBytes(int64(b.N) * rhp.SectorSize)
	b.ResetTimer()

	// upload b.N sectors
	for i := 0; i < b.N; i++ {
		// generate a sector
		sector := make([]byte, rhp.SectorSize)
		frand.Read(sector[:256])

		// upload the sector
		if _, err := session.Append(context.Background(), sector, remainingDuration); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDownload(b *testing.B) {
	renter, host, err := test.NewTestingPair(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer renter.Close()
	defer host.Close()

	if err := host.AddVolume(filepath.Join(b.TempDir(), "storage.dat"), uint64(b.N)); err != nil {
		b.Fatal(err)
	}

	// form a contract
	contract, err := renter.FormContract(context.Background(), host.RHPv2Addr(), host.PublicKey(), types.SiacoinPrecision.Mul64(10), types.SiacoinPrecision.Mul64(20), 200)
	if err != nil {
		b.Fatal(err)
	}

	// mine a block to confirm the contract
	if err := host.MineBlocks(host.WalletAddress(), 1); err != nil {
		b.Fatal(err)
	}

	session, err := renter.NewRHP2Session(context.Background(), host.RHPv2Addr(), host.PublicKey(), contract.ID())
	if err != nil {
		b.Fatal(err)
	}
	defer session.Close()

	// calculate the remaining duration of the contract
	var remainingDuration uint64
	contractExpiration := uint64(session.Revision().NewWindowEnd)
	currentHeight := renter.TipState().Index.Height
	if contractExpiration < currentHeight {
		b.Fatal("contract expired")
	}
	remainingDuration = contractExpiration - currentHeight

	var uploaded []crypto.Hash
	// upload b.N sectors
	for i := 0; i < b.N; i++ {
		// generate a sector
		sector := make([]byte, rhp.SectorSize)
		frand.Read(sector[:256])

		// upload the sector
		root, err := session.Append(context.Background(), sector, remainingDuration)
		if err != nil {
			b.Fatal(err)
		}
		uploaded = append(uploaded, root)
	}

	b.ReportAllocs()
	b.SetBytes(int64(b.N) * rhp.SectorSize)
	b.ResetTimer()

	for _, root := range uploaded {
		// download the sector
		if err := session.Read(context.Background(), io.Discard, root, 0, rhp.SectorSize); err != nil {
			b.Fatal(err)
		}
	}
}
