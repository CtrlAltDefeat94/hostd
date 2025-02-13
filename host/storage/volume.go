package storage

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"sync"

	rhp2 "go.sia.tech/core/rhp/v2"
	"lukechampine.com/frand"
)

type (
	// volumeData wraps the methods needed to read and write sector data to a
	// volume.
	volumeData interface {
		io.ReaderAt
		io.WriterAt

		Sync() error
		Truncate(int64) error
		Close() error
	}

	// A volume stores and retrieves sector data
	volume struct {
		// data is a flatfile that stores the volume's sector data
		data volumeData

		mu    sync.Mutex // protects the fields below
		stats VolumeStats
		// busy must be set to true when the volume is being resized to prevent
		// conflicting operations.
		busy bool
	}

	// VolumeStats contains statistics about a volume
	VolumeStats struct {
		FailedReads      uint64  `json:"failedReads"`
		FailedWrites     uint64  `json:"failedWrites"`
		SuccessfulReads  uint64  `json:"successfulReads"`
		SuccessfulWrites uint64  `json:"successfulWrites"`
		Status           string  `json:"status"`
		Errors           []error `json:"errors"`
	}

	// A Volume stores and retrieves sector data
	Volume struct {
		ID           int64  `json:"ID"`
		LocalPath    string `json:"localPath"`
		UsedSectors  uint64 `json:"usedSectors"`
		TotalSectors uint64 `json:"totalSectors"`
		ReadOnly     bool   `json:"readOnly"`
		Available    bool   `json:"available"`
	}

	// VolumeMeta contains the metadata of a volume.
	VolumeMeta struct {
		Volume
		VolumeStats
	}
)

// ErrVolumeNotAvailable is returned when a volume is not available
var ErrVolumeNotAvailable = errors.New("volume not available")

func (v *volume) appendError(err error) {
	v.stats.Errors = append(v.stats.Errors, err)
	if len(v.stats.Errors) > 100 {
		v.stats.Errors = v.stats.Errors[len(v.stats.Errors)-100:]
	}
}

// OpenVolume opens the volume at localPath
func (v *volume) OpenVolume(localPath string, reload bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data != nil && !reload {
		return nil
	}
	f, err := os.OpenFile(localPath, os.O_RDWR, 0700)
	if err != nil {
		return err
	}
	v.data = f
	return nil
}

// ReadSector reads the sector at index from the volume
func (v *volume) ReadSector(index uint64) (*[rhp2.SectorSize]byte, error) {
	if v.data == nil {
		return nil, ErrVolumeNotAvailable
	}
	var sector [rhp2.SectorSize]byte
	_, err := v.data.ReadAt(sector[:], int64(index*rhp2.SectorSize))
	v.mu.Lock()
	if err != nil {
		v.stats.FailedReads++
		v.appendError(fmt.Errorf("failed to read sector at index %v: %w", index, err))
	} else {
		v.stats.SuccessfulReads++
	}
	v.mu.Unlock()
	return &sector, err
}

// WriteSector writes a sector to the volume at index
func (v *volume) WriteSector(data *[rhp2.SectorSize]byte, index uint64) error {
	if v.data == nil {
		panic("volume not open") // developer error
	}
	_, err := v.data.WriteAt(data[:], int64(index*rhp2.SectorSize))
	v.mu.Lock()
	if err != nil {
		v.stats.FailedWrites++
		v.appendError(fmt.Errorf("failed to write sector to index %v: %w", index, err))
	} else {
		v.stats.SuccessfulWrites++
	}
	v.mu.Unlock()
	return err
}

// SetStatus sets the status message of the volume
func (v *volume) SetStatus(status string) {
	v.mu.Lock()
	v.stats.Status = status
	v.mu.Unlock()
}

// Sync syncs the volume
func (v *volume) Sync() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.data == nil {
		return nil
	}
	err := v.data.Sync()
	if err != nil {
		v.appendError(fmt.Errorf("failed to sync volume: %w", err))
	}
	return err
}

func (v *volume) Resize(oldSectors, newSectors uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.data == nil {
		return ErrVolumeNotAvailable
	}

	if newSectors > oldSectors {
		buf := make([]byte, rhp2.SectorSize)
		r := rand.New(rand.NewSource(int64(frand.Uint64n(math.MaxInt64))))
		for i := oldSectors; i < newSectors; i++ {
			r.Read(buf)
			if _, err := v.data.WriteAt(buf, int64(i*rhp2.SectorSize)); err != nil {
				return fmt.Errorf("failed to write sector to index %v: %w", i, err)
			}
		}
	} else {
		if err := v.data.Truncate(int64(newSectors * rhp2.SectorSize)); err != nil {
			return fmt.Errorf("failed to truncate volume: %w", err)
		}
	}
	return nil
}

// Close closes the volume
func (v *volume) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data == nil {
		return nil
	} else if err := v.data.Sync(); err != nil {
		return fmt.Errorf("failed to sync volume: %w", err)
	} else if err := v.data.Close(); err != nil {
		return fmt.Errorf("failed to close volume: %w", err)
	}
	v.data = nil
	v.stats.Status = VolumeStatusUnavailable
	return nil
}
