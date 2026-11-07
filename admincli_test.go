package main

import "testing"

func TestEnrollmentLink(t *testing.T) {
	server := "https://journal.example.com:56398"
	if got, want := enrollmentLink("K7QM-4XPD", &server, nil), "journal://enroll?code=K7QM-4XPD&server=https%3A%2F%2Fjournal.example.com%3A56398"; got != want {
		t.Errorf("enrollmentLink = %q, want %q", got, want)
	}
	// Without public_url the app keeps the address it has.
	if got, want := enrollmentLink("K7QM-4XPD", nil, nil), "journal://enroll?code=K7QM-4XPD"; got != want {
		t.Errorf("enrollmentLink = %q, want %q", got, want)
	}
	// With who the server is and where: the phone pins the key and can look the server up.
	found := &enrollDiscovery{Fingerprint: "ab12", Key: "cd34", Addresses: []string{"https://203.0.113.7:56398", "https://192.168.1.20:56398"}}
	if got, want := enrollmentLink("K7QM-4XPD", &server, found), "journal://enroll?address=https%3A%2F%2F203.0.113.7%3A56398&address=https%3A%2F%2F192.168.1.20%3A56398&code=K7QM-4XPD&fingerprint=ab12&key=cd34&server=https%3A%2F%2Fjournal.example.com%3A56398"; got != want {
		t.Errorf("enrollmentLink = %q, want %q", got, want)
	}
}
