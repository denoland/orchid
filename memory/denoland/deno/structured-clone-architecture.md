---
name: structured-clone-architecture
description: How Deno makes Web platform objects (Blob, File, CryptoKey, DOMException) survive structuredClone via V8's host_object hook plus a JS-side cloneable-resource registry.
metadata:
  type: reference
---

Global `structuredClone` is exported from `ext/web/13_message_port.js` (not `ext/web/02_structured_clone.js` — that one is a fallback for cases that need explicit deserializers passed at call time). Both ultimately call `core.structuredClone(value)`, which calls `op_structured_clone(value, cloneableDeserializers)` in `libs/core/01_core.js`.

The C++/Rust pipeline lives in `libs/core/ops_builtin_v8.rs`:

- `op_structured_clone` runs V8's `ValueSerializer` with a `SerializeDeserialize` delegate that has `host_object_brand = Some(Symbol.for("Deno.core.hostObject"))`.
- `has_custom_host_object` returns true → V8 calls `is_host_object(object)` for every JS receiver. We answer by `object.has(brand_symbol)`, which walks the prototype chain.
- `write_host_object(object)` reads `object.get(brand_symbol)`, expects a function, calls it with `this = object`. The function returns a plain `{type: "<name>", ...data}` object. We write `u32::MAX` then serialize that metadata.
- After serialize, the same op deserializes immediately with `deserializers` set. `read_host_object` reads the u32, sees `u32::MAX`, reads the metadata object back, looks up `metadata.type` in the deserializers registry, calls that function with the metadata as the argument.

On the JS side, each cloneable class wires two things into the global `core`:

1. `ObjectDefineProperty(Cls.prototype, core.hostObjectBrand, {value: function() { return { type: "Cls", ...fields }; }, enumerable: false, configurable: false, writable: false})`
2. `core.registerCloneableResource("Cls", (data) => { /* rebuild and return new Cls instance */ })`

Symbols used to stash internal slots (e.g. `_type`, `_size`, `_parts` in `ext/web/09_file.js`) are closed over by both the brand function and the deserializer because both live in the same IIFE, so reading and writing the same slot works.

`MessagePort` uses a *string* brand (`port[core.hostObjectBrand] = "MessagePort"`) rather than a function; that path goes through `host_objects` + `transferableResources` instead of `cloneableDeserializers` and is used only for the transfer list, not for plain cloning.

Loading order matters but is taken care of by `runtime/js/98_global_scope_shared.js`, which eagerly `core.loadExtScript("ext:deno_web/09_file.js")` at startup — that runs the IIFE and installs the brand + deserializer before any user code can call `structuredClone`. All `ext/web/*.js` files were moved from `esm` to `lazy_loaded_js` in #33760; this still works as long as the eager `loadExtScript` calls in `98_global_scope_shared.js` keep happening at boot.

History:
- #32672 — added the `hostObjectBrand` / `registerCloneableResource` infrastructure
- #33827 — added Blob and File registrations (closes #12067; fixes the bug #33382 → orchid#28)
