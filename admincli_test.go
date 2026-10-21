package main

import "testing"

func TestEnrollmentLink(t *testing.T) {
	server := "https://journal.example.com:56398"
	if got, want := enrollmentLink("K7QM-4XPD", &server), "journal://enroll?code=K7QM-4XPD&server=https%3A%2F%2Fjournal.example.com%3A56398"; got != want {
		t.Errorf("enrollmentLink = %q, want %q", got, want)
	}
	// Without public_url the app keeps the address it has.
	if got, want := enrollmentLink("K7QM-4XPD", nil), "journal://enroll?code=K7QM-4XPD"; got != want {
		t.Errorf("enrollmentLink = %q, want %q", got, want)
	}
}
