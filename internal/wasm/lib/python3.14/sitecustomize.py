"""WASI adjustments, imported by site at interpreter start-up.

Keep this cheap: it runs on every start. Patches to stdlib modules are
applied lazily, when the module is first imported.
"""

import sys


def _patch_selector_events(module):
    # The selector event loop wakes itself up through a socketpair (the
    # "self pipe") when another thread or a signal handler schedules a
    # callback. WASI has neither threads nor signals, and cannot create
    # socketpairs, so the self pipe is disabled to make asyncio.run() work.
    loop = module.BaseSelectorEventLoop

    def _make_self_pipe(self):
        self._ssock = None
        self._csock = None

    loop._make_self_pipe = _make_self_pipe
    loop._close_self_pipe = lambda self: None
    loop._write_to_self = lambda self: None


def _patch_pypdf_crypto(module):
    from _pyodide_pypdf import install
    install(module)


_PATCHES = {
    "asyncio.selector_events": _patch_selector_events,
    "pypdf._crypt_providers": _patch_pypdf_crypto,
}


class _PatchFinder:
    """Meta path finder that wraps the loader of patched modules."""

    def find_spec(self, name, path, target=None):
        patch = _PATCHES.get(name)
        if patch is None:
            return None
        for finder in sys.meta_path:
            if finder is self:
                continue
            spec = finder.find_spec(name, path, target)
            if spec is not None:
                break
        else:
            return None

        original = spec.loader

        class PatchedLoader:
            def create_module(self, spec):
                return original.create_module(spec)

            def exec_module(self, module):
                original.exec_module(module)
                patch(module)

            def __getattr__(self, attr):
                return getattr(original, attr)

        spec.loader = PatchedLoader()
        return spec


if sys.platform == "wasi":
    sys.meta_path.insert(0, _PatchFinder())
