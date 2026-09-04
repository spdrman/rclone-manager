package config

import (
	"strings"
	"testing"
)

// A configuration this build can validate and can never execute is worse
// than one it refuses, because the refusal an operator eventually gets
// arrives from the move engine, in a log, at the moment a local copy was
// about to be deleted, and `backup-manager check` said "config OK" on the
// way in.
//
// This file is the check for the one such configuration Validate now
// refuses. The other one, a retention tier delivering to an archive
// storage class, is #438: it is equally dead and its refusal invalidates
// the #242 conformance scenario, which is built on exactly that chain.

// TestValidate_AttestedIsRefusedOnAMediumTypeThatCannotAttest is FR-31's
// ladder meeting the only backend there is.
//
// `attested` means the medium states its own full-object digest and this
// product believes it without downloading the object. rclone v1.75.0's s3
// backend reports one hash capability, MD5, and that MD5 is the ETag, so
// there is nothing on an s3 medium that could ever satisfy the class. A
// move to such a medium therefore refuses at the verification step, every
// cycle, forever, and the artifact never arrives.
//
// The refusal has to name the way out, because an operator who wrote
// `attested` wanted the strongest thing available and has to be told what
// that actually is.
func TestValidate_AttestedIsRefusedOnAMediumTypeThatCannotAttest(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums[0].UploadVerification = UploadVerificationAttested

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted upload_verification: attested on an s3 medium. No s3 medium can ever achieve it, " +
			"so this is a configuration `backup-manager check` calls OK and the move engine refuses on every cycle for the life of the deployment")
	}
	for _, want := range []string{
		"storage_mediums[0]",
		UploadVerificationAttested,
		StorageMediumTypeS3,
		UploadVerificationReadback,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say what was refused or what to write instead: %v", want, err)
		}
	}
}

// TestValidate_ReadbackIsStillAccepted is the control the test above needs.
//
// Without it, a Validate that had started refusing every upload_verification
// value, or every medium, would pass the check above for entirely the wrong
// reason.
func TestValidate_ReadbackIsStillAccepted(t *testing.T) {
	for _, verification := range []string{"", UploadVerificationReadback} {
		c := mediumsConfig()
		c.StorageMediums[0].UploadVerification = verification
		if err := c.Validate(); err != nil {
			t.Errorf("Validate refused upload_verification %q, which is the mode that works: %v", verification, err)
		}
	}
}

// TestValidate_AnUnknownMediumTypeIsReportedOnceAndNotTwice keeps the
// attested refusal from burying a mistake under a consequence of itself,
// the same discipline validateStorageMediums already applies to a
// malformed medium id.
//
// A medium whose type this build has no backend for cannot attest either,
// but saying so would send an operator to the upload_verification key when
// the thing they got wrong is the type.
func TestValidate_AnUnknownMediumTypeIsReportedOnceAndNotTwice(t *testing.T) {
	c := mediumsConfig()
	c.StorageMediums[0].Type = "azure"
	c.StorageMediums[0].UploadVerification = UploadVerificationAttested

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted type: azure")
	}
	if strings.Contains(err.Error(), UploadVerificationAttested) {
		t.Errorf("the unknown type also produced an attestation complaint, which points at the wrong key: %v", err)
	}
}
