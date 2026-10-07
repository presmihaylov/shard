package bundle

import "testing"

// The list is the contract every checkpoint carries, so the test pins the exact value and not a sample of it.
func TestCPUFeaturesCapsAnX86GuestAndLeavesAnArm64OneAlone(t *testing.T) {
	const want = "fpu,vme,de,pse,tsc,msr,pae,mce,cx8,apic,sep,mtrr,pge,mca,cmov,pat,pse36,clflush,mmx,fxsr," +
		"sse,sse2,ht,syscall,nx,rdtscp,lm,pni,pclmulqdq,ssse3,fma,cx16,sse4_1,sse4_2,movbe,popcnt,aes," +
		"xsave,osxsave,avx,f16c,rdrand,lahf_lm,abm,fsgsbase,bmi1,avx2,bmi2,rdseed,adx,xsaveopt,xsavec,xgetbv1,xsaves"
	if got, ok := cpuFeaturesFor("amd64"); !ok || got != want {
		t.Errorf("amd64: the guest sees %q, %v, want the pinned list %q", got, ok, want)
	}
	if got, ok := cpuFeaturesFor("arm64"); ok {
		t.Errorf("arm64: got the annotation %q, want none", got)
	}
}
