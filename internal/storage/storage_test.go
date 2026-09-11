package storage

import (
	"strings"
	"testing"
)

func TestValidateRejectsUnknownPurpose(t *testing.T) {
	if err := Validate("invented", "application/pdf", 100); err == nil {
		t.Fatal("an unknown purpose must be refused: it decides the key prefix")
	}
}

func TestValidateAcceptsContentTypeWithParameters(t *testing.T) {
	// Browsers append "; charset=..." and a strict equality check would
	// reject a legitimate upload.
	if err := Validate(PurposeAgreementDoc, "application/pdf; charset=binary", 1024); err != nil {
		t.Fatalf("media type with parameters should be accepted: %v", err)
	}
}

func TestValidateEnforcesTheSizeLimit(t *testing.T) {
	if err := Validate(PurposeTruckPhoto, "image/jpeg", 11<<20); err == nil {
		t.Fatal("a photo over the limit must be refused at signing time")
	}
	if err := Validate(PurposeTruckPhoto, "image/jpeg", 1<<20); err != nil {
		t.Fatalf("a photo under the limit should pass: %v", err)
	}
}

func TestValidateRejectsDisallowedType(t *testing.T) {
	if err := Validate(PurposeTruckPhoto, "text/html", 1024); err == nil {
		t.Fatal("text/html must never be accepted: it is the whole reason for an allow list")
	}
}

func TestKeyIsScopedToTheCompanyAndIgnoresTheCallersFilename(t *testing.T) {
	key, err := Key("acme", PurposeOrderPOD, "../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "acme/order-pod/") {
		t.Fatalf("key must sit under the company and purpose, got %q", key)
	}
	if strings.Contains(key, "..") || strings.Contains(key, "passwd") {
		t.Fatalf("the caller's filename must not reach the key, got %q", key)
	}
}

func TestKeysDoNotCollide(t *testing.T) {
	a, _ := Key("acme", PurposeOrderPOD, "pod.jpg")
	b, _ := Key("acme", PurposeOrderPOD, "pod.jpg")
	if a == b {
		t.Fatal("two drivers photographing pod.jpg must not overwrite each other")
	}
}

func TestOwnedBy(t *testing.T) {
	key, _ := Key("acme", PurposeAgreementDoc, "contract.pdf")
	if !OwnedBy(key, "acme") {
		t.Fatal("a company must own its own key")
	}
	if OwnedBy(key, "other") {
		t.Fatal("a key must not be readable by another company")
	}
	if OwnedBy(key, "") {
		t.Fatal("an empty company must never own anything")
	}
	// The prefix check must not match a company whose id merely starts the same.
	if OwnedBy("acme-corp/order-pod/x.jpg", "acme") {
		t.Fatal("acme must not own acme-corp's files")
	}
}

func TestUnconfiguredClientRefusesRatherThanPanics(t *testing.T) {
	c := &Client{}
	if c.Configured() {
		t.Fatal("a client with no bucket is not configured")
	}
	if _, _, err := c.PresignPut(t.Context(), "k", "image/jpeg"); err != ErrNotConfigured {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
