package config

import "testing"

func TestRenameSite(t *testing.T) {
	s := &State{Sites: []Site{{Name: "alpha"}, {Name: "beta"}}}
	if err := s.RenameSite("beta", "My App"); err != nil {
		t.Fatal(err)
	}
	if s.FindSite("my-app") == nil || s.FindSite("beta") != nil {
		t.Fatalf("rename failed: %+v", s.Sites)
	}
	if s.Sites[0].Name != "alpha" || s.Sites[1].Name != "my-app" {
		t.Fatalf("order wrong: %+v", s.Sites)
	}
	if err := s.RenameSite("my-app", "alpha"); err == nil {
		t.Fatal("duplicate rename must fail")
	}
	if err := s.RenameSite("ghost", "x"); err == nil {
		t.Fatal("renaming a missing site must fail")
	}
	if err := s.RenameSite("alpha", "!!!"); err == nil {
		t.Fatal("invalid name must fail")
	}
}
