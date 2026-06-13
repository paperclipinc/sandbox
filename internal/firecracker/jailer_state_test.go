package firecracker

import "testing"

func TestJailerStateJailed(t *testing.T) {
	c := &Client{
		chrootDir:   "/srv/jailer/firecracker/vm-1/root",
		jailerVMDir: "/srv/jailer/firecracker/vm-1",
		jailedUID:   100222,
	}
	st := c.JailerState()
	if st.ChrootDir != "/srv/jailer/firecracker/vm-1/root" {
		t.Errorf("ChrootDir = %q", st.ChrootDir)
	}
	if st.JailerVMDir != "/srv/jailer/firecracker/vm-1" {
		t.Errorf("JailerVMDir = %q", st.JailerVMDir)
	}
	if st.JailedUID != 100222 {
		t.Errorf("JailedUID = %d", st.JailedUID)
	}
}

func TestJailerStateDirectExec(t *testing.T) {
	c := &Client{workDir: "/var/lib/mitos/sandboxes/a"}
	st := c.JailerState()
	if st.ChrootDir != "" || st.JailerVMDir != "" || st.JailedUID != 0 {
		t.Errorf("direct-exec client must report zero jailer state, got %+v", st)
	}
}
