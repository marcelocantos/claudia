// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
// Read-only probe. Never print password bytes, attributes, or error descriptions.
#include <Security/Security.h>
#include <stdio.h>
#include <string.h>

static int report(const char *stage, OSStatus status) {
    printf("%s %d\n", stage, (int)status);
    return 0;
}

int main(int argc, char **argv) {
    if (argc != 3) return 2;
    OSStatus status = SecKeychainSetUserInteractionAllowed(false);
    if (status != errSecSuccess) return report("interaction", status);
    SecKeychainItemRef item = NULL;
    // Omitting password output retrieves a reference without reading the secret.
    status = SecKeychainFindGenericPassword(NULL,
        (UInt32)strlen(argv[1]), argv[1], (UInt32)strlen(argv[2]), argv[2],
        NULL, NULL, &item);
    if (status != errSecSuccess) return report("lookup", status);
    SecKeychainRef keychain = NULL;
    status = SecKeychainItemCopyKeychain(item, &keychain);
    if (status != errSecSuccess) { CFRelease(item); return report("keychain", status); }
    SecKeychainStatus state = 0;
    status = SecKeychainGetStatus(keychain, &state);
    CFRelease(keychain);
    if (status != errSecSuccess) { CFRelease(item); return report("state", status); }
    if (!(state & kSecUnlockStateStatus)) { CFRelease(item); return report("locked", 0); }
    UInt32 length = 0;
    void *data = NULL;
    status = SecKeychainItemCopyContent(item, NULL, NULL, &length, &data);
    if (data != NULL) {
        volatile unsigned char *p = data;
        for (UInt32 i = 0; i < length; ++i) p[i] = 0;
        SecKeychainItemFreeContent(NULL, data);
    }
    CFRelease(item);
    return report("read", status);
}
