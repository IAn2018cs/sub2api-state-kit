#!/usr/bin/env python3
"""Prepare a separate, pinned Sub2API 0.2.7 checkout with the optional resource RPCs."""
import argparse
from pathlib import Path
import subprocess

BASE = 'aea725f2ea644d5592d0bbb1d63b607efa7e200a'
ROOT = Path(__file__).resolve().parents[1]

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--upstream', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    upstream, output = args.upstream.resolve(), args.output.resolve()
    if output.exists():
        parser.error('output must not already exist')
    subprocess.run(['git', '-C', str(upstream), 'cat-file', '-e', BASE + '^{commit}'], check=True)
    patch = ROOT / 'plugin/host-adapter/sub2api-v0.2.7-resource-directory.patch'
    subprocess.run(['git', 'clone', '--no-hardlinks', '--no-checkout', str(upstream), str(output)], check=True)
    subprocess.run(['git', '-C', str(output), 'checkout', '--detach', BASE], check=True)
    subprocess.run(['git', '-C', str(output), 'apply', '--check', str(patch)], check=True)
    subprocess.run(['git', '-C', str(output), 'apply', str(patch)], check=True)
    print('Prepared optional resource-directory host:', output)
    print('No services started or configuration/data copied. See docs/plugin-host-directory.md.')

if __name__ == '__main__':
    main()
