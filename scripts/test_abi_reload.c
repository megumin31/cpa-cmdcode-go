/* Exercise the real C ABI, including Go shared-library NODELETE reuse.
 * This harness never executes a model, reads credentials, or calls upstream.
 */
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    int (*call)(void *, const char *, const uint8_t *, size_t, buffer *);
    void (*free_buffer)(void *, size_t);
} host_api;
typedef struct {
    uint32_t abi_version;
    int (*call)(char *, uint8_t *, size_t, buffer *);
    void (*free_buffer)(void *, size_t);
    void (*shutdown)(void);
} plugin_api;
typedef int (*init_fn)(const host_api *, plugin_api *);

static void fail(const char *message) {
    fprintf(stderr, "ABI reload regression: %s\n", message);
    exit(EXIT_FAILURE);
}

static int host_call(void *ctx, const char *method, const uint8_t *request,
                     size_t request_len, buffer *response) {
    (void)ctx; (void)method; (void)request; (void)request_len;
    memset(response, 0, sizeof(*response));
    fail("unexpected host callback during offline lifecycle test");
    return 1;
}

static void host_free(void *ptr, size_t len) {
    (void)len;
    free(ptr);
}

static void expect_call(plugin_api *api, const char *method, const char *request,
                        int expect_success, const char *contains) {
    buffer out = {0};
    int rc = api->call((char *)method, (uint8_t *)request, strlen(request), &out);
    if (!out.ptr || out.len == 0 || out.len > 1024 * 1024)
        fail("call returned an absent or oversized response");
    char *body = malloc(out.len + 1);
    if (!body) fail("malloc failed");
    memcpy(body, out.ptr, out.len);
    body[out.len] = '\0';
    api->free_buffer(out.ptr, out.len);
    if ((rc == 0) != expect_success || !strstr(body, contains)) {
        fprintf(stderr, "%s returned rc=%d body=%s; expected success=%d and %s\n",
                method, rc, body, expect_success, contains);
        free(body);
        fail("unexpected call result");
    }
    free(body);
}

int main(int argc, char **argv) {
    if (argc != 2) fail("usage: test_abi_reload /absolute/path/plugin.so");
    const char *expected_version = getenv("EXPECTED_PLUGIN_VERSION");
    char version_match[128];
    if (expected_version && snprintf(version_match, sizeof(version_match),
                                    "\"Version\":\"%s\"", expected_version) >= (int)sizeof(version_match))
        fail("expected version is too long");
    const char *route = "{\"RequestedModel\":\"cmdcode-go/deepseek-v4-flash\"}";
    const char *configure = "{\"config_yaml\":\"models_refresh_interval: '0'\\ndisable_models: [cmdcode-go/deepseek-v4-flash]\"}";
    host_api host = {1, NULL, host_call, host_free};
    for (int round = 0; round < 3; ++round) {
        /* Intentionally use the exact same pathname every time. */
        void *handle = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
        if (!handle) fail(dlerror());
        dlerror();
        void *symbol = dlsym(handle, "cliproxy_plugin_init");
        const char *error = dlerror();
        if (error) fail(error);
        init_fn init = NULL;
        _Static_assert(sizeof(init) == sizeof(symbol), "incompatible dlsym ABI");
        memcpy(&init, &symbol, sizeof(init));
        plugin_api api = {0};
        host_api invalid_host = host;
        invalid_host.abi_version = 999;
        if (init(NULL, &api) == 0 || init(&host, NULL) == 0 ||
            init(&invalid_host, &api) == 0)
            fail("invalid initialization was accepted");
        if (init(&host, &api) != 0) fail("valid initialization failed");
        if (api.abi_version != 1 || !api.call || !api.free_buffer || !api.shutdown)
            fail("initialization returned an incomplete function table");
        expect_call(&api, "executor.identifier", "{}", 1, "\"identifier\":\"cmdcode-go\"");
        expect_call(&api, "model.route", route, 1, "\"Handled\":true");
        expect_call(&api, "plugin.reconfigure", configure, 1, "\"ok\":true");
        if (expected_version)
            expect_call(&api, "plugin.reconfigure", configure, 1, version_match);
        expect_call(&api, "model.route", route, 1, "\"Handled\":false");
        plugin_api duplicate = {0};
        if (init(&host, &duplicate) == 0) fail("double initialization was accepted");
        expect_call(&api, "model.route", route, 1, "\"Handled\":false");
        api.shutdown();
        api.shutdown();
        expect_call(&api, "executor.identifier", "{}", 0, "\"ok\":false");
        api.free_buffer(NULL, 0);
        if (dlclose(handle) != 0) fail(dlerror());
        printf("ABI same-path reload round %d passed\n", round + 1);
    }
    return EXIT_SUCCESS;
}
