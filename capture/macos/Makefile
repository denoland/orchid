SHELL := /bin/bash

APP_NAME    ?= OrchidCapture
BUNDLE_ID   ?= land.deno.orchid.capture
TEAM_ID     ?=
SIGN_ID     ?= $(if $(TEAM_ID),Developer ID Application: ($(TEAM_ID)),-)
# An Apple ID with an app-specific password (xcrun notarytool store-credentials
# stores these into the keychain under PROFILE_NAME for later --keychain-profile).
NOTARY_PROFILE ?= orchid-notarize

BUILD_DIR := .build
RELEASE_BIN := $(BUILD_DIR)/release/$(APP_NAME)
APP_BUNDLE := $(BUILD_DIR)/$(APP_NAME).app
APP_BINARY := $(APP_BUNDLE)/Contents/MacOS/$(APP_NAME)
APP_PLIST  := $(APP_BUNDLE)/Contents/Info.plist
ZIP        := $(BUILD_DIR)/$(APP_NAME).zip

.PHONY: run build app sign notarize staple bundle dmg clean help

help:
	@printf "targets:\n"
	@printf "  make run        — swift run (no bundling, dock will flash on launch)\n"
	@printf "  make app        — build a real .app bundle under $(APP_BUNDLE)\n"
	@printf "  make sign       — codesign the bundle with SIGN_ID (defaults to ad-hoc -)\n"
	@printf "  make notarize   — submit to Apple notary; requires NOTARY_PROFILE\n"
	@printf "  make staple     — staple the notarization ticket onto the bundle\n"
	@printf "  make dmg        — convenience zip of the signed app for distribution\n"
	@printf "\nrequired env for distribution:\n"
	@printf "  TEAM_ID         — Apple Developer Team ID (10 chars)\n"
	@printf "  SIGN_ID         — codesign identity (default Developer ID Application: ...)\n"
	@printf "  NOTARY_PROFILE  — keychain profile name from notarytool store-credentials\n"

run:
	swift run -c release

build: $(RELEASE_BIN)

$(RELEASE_BIN):
	swift build -c release

# Assemble a real .app bundle around the Swift Package executable so
# Finder treats it as an app and macOS honours LSUIElement.
app: $(APP_BINARY)
$(APP_BINARY): $(RELEASE_BIN) Info.plist | $(APP_BUNDLE)
	cp $(RELEASE_BIN) $(APP_BINARY)
	cp Info.plist $(APP_PLIST)
$(APP_BUNDLE):
	mkdir -p $(APP_BUNDLE)/Contents/MacOS
	mkdir -p $(APP_BUNDLE)/Contents/Resources

sign: app
	codesign --force --options runtime --timestamp \
		--sign "$(SIGN_ID)" \
		--entitlements Entitlements.plist \
		$(APP_BUNDLE)
	codesign --verify --strict --verbose=2 $(APP_BUNDLE)

# `xcrun notarytool store-credentials $(NOTARY_PROFILE)` once per machine to
# set up the profile (Apple ID, app-specific password, team ID).
notarize: sign $(ZIP)
	xcrun notarytool submit $(ZIP) \
		--keychain-profile $(NOTARY_PROFILE) \
		--wait

staple:
	xcrun stapler staple $(APP_BUNDLE)
	xcrun stapler validate $(APP_BUNDLE)
	@printf "✓ %s ready for distribution\n" "$(APP_BUNDLE)"

$(ZIP): $(APP_BUNDLE)
	cd $(BUILD_DIR) && ditto -c -k --keepParent $(APP_NAME).app $(APP_NAME).zip

dmg: $(ZIP)

clean:
	rm -rf $(BUILD_DIR)
