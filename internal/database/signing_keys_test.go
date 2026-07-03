package database

import "testing"

func TestSigningKeyRoundTripAndIdempotency(t *testing.T) {
	requireDB(t)

	const purpose = "terraform-provider-test"

	// Absent to start.
	if _, _, found, err := testDB.GetSigningKey(ctx(), purpose); err != nil || found {
		t.Fatalf("expected no key, got found=%v err=%v", found, err)
	}

	if err := testDB.InsertSigningKeyIfAbsent(ctx(), purpose, "ARMOR-1", "KEYID1"); err != nil {
		t.Fatal(err)
	}

	// A second insert must not overwrite (models the replica race).
	if err := testDB.InsertSigningKeyIfAbsent(ctx(), purpose, "ARMOR-2", "KEYID2"); err != nil {
		t.Fatal(err)
	}

	armor, keyID, found, err := testDB.GetSigningKey(ctx(), purpose)
	if err != nil || !found {
		t.Fatalf("expected key, found=%v err=%v", found, err)
	}
	if armor != "ARMOR-1" || keyID != "KEYID1" {
		t.Errorf("key was overwritten: armor=%q key_id=%q", armor, keyID)
	}
}
