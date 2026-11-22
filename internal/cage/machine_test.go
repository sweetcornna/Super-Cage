package cage

import (
	"math/rand/v2"
	"testing"
)

func TestAutomaticSizeDoesNotMoveWithTheBalloon(t *testing.T) {
	// An 8 GiB workspace VM (MemTotal 7.75 GiB, 8 GiB in memory blocks).
	const total = uint64(8126464) << 10
	if memory, cpus := automaticLimits(RoleDedicated, total/gib*gib, 6); memory != 6 || cpus != 5 {
		t.Fatalf("unballooned %d/%d", memory, cpus)
	}
	random := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		blocks := uint64(2+random.IntN(62)) * gib
		// The kernel keeps 1-4% and some MiB of the blocks out of MemTotal.
		total := blocks - blocks/100*uint64(1+random.IntN(4)) - uint64(random.IntN(200))*mib
		// host.MemGiB without a balloon: MemTotal in whole GiB.
		want, _ := automaticLimits(RoleDedicated, total/gib*gib, 6)
		for balloon := uint64(0); balloon+gib < total; balloon += 256 * mib {
			// The balloon driver reports which kind of kernel this is: exact.
			for _, boot := range []uint64{bootBytes(total-balloon, balloon, blocks, false, true), bootBytes(total, balloon, blocks, true, true)} {
				if memory, _ := automaticLimits(RoleDedicated, boot/gib*gib, 6); memory != want {
					t.Fatalf("blocks %d MiB, MemTotal %d MiB, balloon %d MiB: boot %d MiB gives %d, want %d", blocks>>20, total>>20, balloon>>20, boot>>20, memory, want)
				}
			}
			// Without the driver's report, the blocks: exact for a shrinking
			// kernel, and within the kernel's reservation for a keeping one.
			if boot := bootBytes(total-balloon, balloon, blocks, false, false); boot != total {
				t.Fatalf("shrinking kernel without features: %d", boot)
			}
			if boot := bootBytes(total, balloon, blocks, false, false); boot < total || boot > blocks {
				t.Fatalf("keeping kernel without features: %d", boot)
			}
		}
	}
}
