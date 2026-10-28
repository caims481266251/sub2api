#!/usr/bin/python3
"""Compatibility marker for installed v2 updater image fingerprints.
The resident kernel now connects to its bound proxy directly. No bridge runs.
"""
if __name__ == '__main__':
    raise SystemExit('proxy_bridge_retired_use_bound_kernel_proxy')
