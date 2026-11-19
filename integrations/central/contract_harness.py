"""Execute the standalone Runway app’s actual lease code without a Frappe site.

Only document storage, fencing and time are substituted. This exercises the
route, transition implementation and snapshot, not database/auth correctness.
One JSON request on stdin contains a lease snapshot and the RPC fields.
"""

import ast
import json
import os
import re
from pathlib import Path
import sys
from types import SimpleNamespace

APP_ENV = 'RUNWAY_FRAPPE_APP'


def app_file(path):
    return (Path(os.environ[APP_ENV]) / path).read_text()


def load_functions(path, names, namespace):
    # Parse only the tested functions. The full Runway app can use Python 3.14
    # syntax; the lease contract itself can be exercised with older Python 3.
    source = app_file(path)
    starts = list(re.finditer(r'^def (\w+)\(', source, re.MULTILINE))
    definitions = []
    for start in starts:
        if start.group(1) not in names:
            continue
        following = re.search(r'^def |^@', source[start.end():], re.MULTILINE)
        end = start.end() + following.start() if following else len(source)
        tree = ast.parse(source[start.start():end], filename=path)
        definitions.extend(node for node in tree.body if isinstance(node, ast.FunctionDef))
    if {node.name for node in definitions} != set(names):
        raise ValueError(f'Missing contract functions in {path}')
    exec(compile(ast.Module(body=definitions, type_ignores=[]), path, 'exec'), namespace)


class Document(dict):
    def __getattr__(self, name):
        return self.get(name)

    def __setattr__(self, name, value):
        self[name] = value

    def set(self, name, value):
        self[name] = value


class ContractError(ValueError):
    pass


def reject(message):
    raise ContractError(message)


def update(lease, fields):
    # Storage columns come from the actual DocType, so a missing migration field
    # cannot be hidden by an unrestricted dictionary adapter.
    schema = json.loads(app_file('runway/runway/doctype/runner_lease/runner_lease.json'))
    columns = {field['fieldname'] for field in schema['fields']}
    document = Document({key: value for key, value in lease.items() if key != 'id'})
    document.name = lease['id']

    def save(document):
        unknown = set(document) - columns - {'name'}
        if unknown:
            raise ContractError(f'Unknown Runner Lease fields: {sorted(unknown)}')

    namespace = {'frappe': SimpleNamespace(throw=reject), '_': lambda value: value,
                 'fenced_lease': lambda *args: (Document(name='pool'), document),
                 'save_internal': save, 'iso_utc': lambda value: value,
                 'now_datetime': lambda: '2026-10-09T12:00:00+00:00'}
    load_functions('runway/pools.py', ['update_lease', 'lease_snapshot'], namespace)
    route = {'runway': SimpleNamespace(update_lease=namespace['update_lease'])}
    load_functions('runway/api.py', ['update_lease'], route)
    return route['update_lease'](**fields)


if __name__ == '__main__':
    if not os.environ.get(APP_ENV):
        sys.exit(f'{APP_ENV} must point to the Runway Frappe app checkout')
    try:
        request = json.load(sys.stdin)
        if 'pool' in request:
            namespace = {'frappe': SimpleNamespace(db=SimpleNamespace(get_value=lambda *args, **kwargs: request['region'])),
                         'iso_utc': lambda value: value, 'active_leases': lambda **kwargs: []}
            load_functions('runway/pools.py', ['pool_snapshot'], namespace)
            result = namespace['pool_snapshot'](Document(request['pool']))
        else:
            result = update(request['lease'], request['fields'])
        print(json.dumps({'message': result}))
    except ContractError as error:
        print(json.dumps({'error': str(error)}))
        sys.exit(1)
