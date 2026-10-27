// Package livetest exercises the SDK against the live API. Its tests run
// only when ORIGINCLOUD_LIVE_ZONE names a zone to work in,
// projects/{project}/zones/{zone}, with credentials found the usual way
// (see the core package). They create resources there and delete them
// again.
package livetest

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"go.origincloud.eu/sdk/core"
	pvsv1beta1 "go.origincloud.eu/sdk/pvs/v1beta1"
)

func liveZone(t *testing.T) string {
	zone := os.Getenv("ORIGINCLOUD_LIVE_ZONE")
	if zone == "" {
		t.Skip("ORIGINCLOUD_LIVE_ZONE isn't set")
	}
	return zone
}

// A volume is created, read straight away, grown and deleted, waiting on
// each operation.
func TestVolumeLifecycle(t *testing.T) {
	zone := liveZone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	client, err := core.NewClient(ctx, core.WithUserAgent("origincloud-sdk-go-livetest"))
	if err != nil {
		t.Fatal(err)
	}
	volumes := pvsv1beta1.NewVolumeClient(client)

	op, err := volumes.CreateVolume(ctx, &pvsv1beta1.CreateVolumeRequest{
		Parent: zone,
		Volume: &pvsv1beta1.Volume{SizeGib: 1, Tags: map[string]string{"origin:name": "sdk-go-livetest"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the create: %v", err)
	}
	t.Logf("created %s", created.GetName())
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if op, err := volumes.DeleteVolume(ctx, &pvsv1beta1.DeleteVolumeRequest{Name: created.GetName()}); err != nil {
			t.Errorf("cleanup: deleting %s: %v", created.GetName(), err)
		} else if _, err := op.Wait(ctx); err != nil {
			t.Errorf("cleanup: waiting for the delete of %s: %v", created.GetName(), err)
		}
	})
	if created.GetState() != pvsv1beta1.Volume_AVAILABLE || created.GetSizeGib() != 1 {
		t.Fatalf("created %v", created)
	}

	got, err := volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: created.GetName()})
	if err != nil {
		t.Fatalf("get right after the create: %v", err)
	}
	if got.GetState() != pvsv1beta1.Volume_AVAILABLE {
		t.Fatalf("got %v right after the create", got)
	}

	op, err = volumes.UpdateVolume(ctx, &pvsv1beta1.UpdateVolumeRequest{
		Volume:     &pvsv1beta1.Volume{Name: created.GetName(), SizeGib: 2},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size_gib"}},
	})
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	resized, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("waiting for the resize: %v", err)
	}
	if resized.GetSizeGib() != 2 {
		t.Fatalf("resized to %d GiB, want 2", resized.GetSizeGib())
	}
	got, err = volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: created.GetName()})
	if err != nil || got.GetSizeGib() != 2 {
		t.Fatalf("get right after the resize: %v, %v", got, err)
	}

	deleteOp, err := volumes.DeleteVolume(ctx, &pvsv1beta1.DeleteVolumeRequest{Name: created.GetName()})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := deleteOp.Wait(ctx); err != nil {
		t.Fatalf("waiting for the delete: %v", err)
	}
	deleted = true
	got, err = volumes.GetVolume(ctx, &pvsv1beta1.GetVolumeRequest{Name: created.GetName()})
	if err != nil || got.GetState() != pvsv1beta1.Volume_DELETED {
		t.Fatalf("get right after the delete: %v, %v", got, err)
	}

	for volume, err := range volumes.AllVolumes(ctx, &pvsv1beta1.ListVolumesRequest{Parent: zone}) {
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if volume.GetName() == created.GetName() {
			t.Fatalf("the deleted volume %s is listed", volume.GetName())
		}
	}
}
