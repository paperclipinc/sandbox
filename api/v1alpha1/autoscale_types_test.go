package v1alpha1

import "testing"

func TestPoolAutoscaleSpecDeepCopy(t *testing.T) {
	min := int32(1)
	max := int32(10)
	spare := int32(3)
	cd := int32(120)
	in := &SandboxPool{
		Spec: SandboxPoolSpec{
			Replicas: 2,
			Autoscale: &PoolAutoscaleSpec{
				MinWarm:                  min,
				MaxWarm:                  max,
				TargetSpare:              spare,
				ScaleDownCooldownSeconds: cd,
			},
		},
	}
	out := in.DeepCopy()
	if out.Spec.Autoscale == in.Spec.Autoscale {
		t.Fatal("DeepCopy must allocate a new Autoscale pointer, got the same pointer")
	}
	if out.Spec.Autoscale.MaxWarm != max || out.Spec.Autoscale.TargetSpare != spare {
		t.Fatalf("DeepCopy lost field values: %+v", out.Spec.Autoscale)
	}
	// Mutating the copy must not affect the original.
	out.Spec.Autoscale.MaxWarm = 99
	if in.Spec.Autoscale.MaxWarm != max {
		t.Fatal("DeepCopy did not isolate Autoscale from the original")
	}
}
