"""Exercise the actual lease contract shipped in the standalone Runway app."""

import importlib.util
import json
import os
from pathlib import Path
import unittest
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location(
    'central_contract', Path(__file__).resolve().parents[1] / 'integrations/central/contract_harness.py'
)
contract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(contract)


@unittest.skipUnless(os.environ.get(contract.APP_ENV), "Set RUNWAY_FRAPPE_APP to the Runway Frappe app checkout")
class CentralContractTests(unittest.TestCase):
    def lease(self, **changes):
        return {'id': 'lease', 'phase': 'running', 'runner_id': 42,
                'runner_name': 'runway-lease', 'request_id': 77,
                'job_id': 'b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e',
                'workflow_run_id': '4242', 'repository': 'BreadGenie/frappe',
                'request_labels': 'self-hosted\nrunway-dogfood',
                'deadline': '2026-10-09T13:00:00+00:00', 'jit_state': 'ready',
                'last_error': 'previous diagnostics unavailable', **changes}

    def update(self, lease, event='started', **fields):
        return contract.update(lease, {'pool': 'pool', 'owner_id': 'owner', 'fence': 1,
                                      'lease_id': lease['id'], 'event': event, **fields})

    def test_runner_dispatch_persists_catalogue_request_before_reconciliation(self):
        pool = contract.Document(region='blr', status='Active', plan='plan', offering='offering',
                                 image_id='image', ssh_public_key='public')
        lease = contract.Document(name='lease', runner_name='runner', phase='reserved', deadline=2)
        committed = []

        def reconcile(document, name):
            self.assertIs(document, pool)
            self.assertEqual(name, lease.name)
            self.assertEqual(committed, [dict(lease)])

        namespace = {
            'frappe': SimpleNamespace(db=SimpleNamespace(commit=lambda: committed.append(dict(lease))),
                                      get_doc=lambda *_: lease),
            'fenced_lease': lambda *_: (pool, lease),
            'get_datetime': lambda value: value, 'now_datetime': lambda: 1,
            'json': json, 'save_internal': lambda lease: None, 'reconcile_lease': reconcile,
            'lease_snapshot': dict,
        }
        contract.load_functions('runway/vms.py', ['ensure_vm'], namespace)
        result = namespace['ensure_vm']('pool', 'owner', 1, lease.name)
        self.assertEqual(result['central_request_key'], lease.name)
        self.assertEqual(json.loads(result['central_create_payload']), {
            'request_key': lease.name, 'region': 'blr', 'title': 'runner', 'hostname': 'runner',
            'plan': 'plan', 'offering': 'offering', 'image_id': 'image', 'ssh_keys': ['public'],
            'is_firewall_enabled': False,
        })

    def test_uncertain_dispatch_reuses_the_saved_payload(self):
        requests = []

        def server(method, **values):
            requests.append((method, values))
            if len(requests) == 1:
                raise ConnectionError('lost reply')
            return {'action': 'create-1', 'status': 'Queued', 'resource_id': None}

        namespace = {'json': json, 'for_pool': lambda pool: SimpleNamespace(server=server)}
        contract.load_functions('runway/vms.py', ['reconcile_vm'], namespace)
        lease = contract.Document(central_request_key='lease',
                                  central_create_payload=json.dumps({'request_key': 'lease', 'plan': 'original'}))
        pool = contract.Document(plan='changed')
        reconcile = namespace['reconcile_vm']
        with self.assertRaises(ConnectionError):
            reconcile(pool, lease)
        reconcile(pool, lease)
        self.assertEqual(requests, [('create_server', {
            'write': True, 'request_key': 'lease', 'plan': 'original',
        })] * 2)
        self.assertEqual(lease.central_create_action, 'create-1')

    def test_runner_identity_storage_accepts_github_ids_above_signed_int32(self):
        schema = json.loads(contract.app_file(
            'runway/runway/doctype/runner_lease/runner_lease.json'))
        field = next(field for field in schema['fields'] if field['fieldname'] == 'runner_id')
        self.assertTrue(field['fieldtype'] == 'Long Int' or
                        field['fieldtype'] == 'Int' and field.get('length', 0) > 11)
        result = self.update(self.lease(runner_id=2**31 + 1), runner_id=2**31 + 1)
        self.assertEqual(result['runner_id'], 2**31 + 1)

    def test_identity_cannot_be_replaced_through_the_route(self):
        for field, value in {'runner_id': 99, 'job_id': 'another-job', 'workflow_run_id': '999',
                             'request_id': 88, 'repository': 'other/repo',
                             'request_labels': 'another-label'}.items():
            with self.subTest(field=field), self.assertRaisesRegex(contract.ContractError, 'identity'):
                self.update(self.lease(), **{field: value})

    def test_missing_evidence_and_replay_preserve_identity_and_errors(self):
        lease = self.lease()
        for _ in range(2):
            lease = self.update(lease, request_id=0)
        for field in ('request_id', 'workflow_run_id', 'repository', 'request_labels', 'last_error'):
            self.assertEqual(lease[field], self.lease()[field])

    def test_zero_request_direct_assignment_can_record_evidence(self):
        lease = self.lease(request_id=0, job_id='', repository='', request_labels='')
        result = self.update(lease, request_id=0, job_id=self.lease()['job_id'],
                             repository='BreadGenie/frappe', request_labels='runway-dogfood')
        self.assertEqual(result['request_id'], 0)
        self.assertEqual(result['repository'], 'BreadGenie/frappe')
        self.assertEqual(result['request_labels'], 'runway-dogfood')

    def test_acquisition_requires_positive_id_and_durable_intent(self):
        for value in (-1, 0, 2**63):
            with self.subTest(request=value), self.assertRaises(contract.ContractError):
                self.update(self.lease(request_id=0), 'acquire_requested', request_id=value)
        with self.assertRaisesRegex(contract.ContractError, 'intent'):
            self.update(self.lease(request_id=0), 'acquired')
        lease = self.update(self.lease(request_id=0, phase='vm_ready'),
                            'acquire_requested', request_id=77)
        self.assertEqual(lease['acquire_state'], 'pending')
        lease = self.update(lease, 'acquired')
        lease = self.update(lease, 'acquire_requested', request_id=77)
        self.assertEqual(lease['acquire_state'], 'acquired')

    def test_retiring_lease_cannot_acquire_or_be_revived(self):
        for phase in ('deleting', 'deleted'):
            with self.subTest(phase=phase), self.assertRaises(contract.ContractError):
                self.update(self.lease(phase=phase), 'acquire_requested', request_id=77)
        lease = self.update(self.lease(phase='deleted'), 'completed', result='success')
        self.assertEqual(lease['phase'], 'deleted')
        self.assertEqual(lease['last_error'], self.lease()['last_error'])

    def test_rejected_phase_uses_the_validation_error(self):
        # Guards must call Frappe's translation function, not the pool document
        # accidentally assigned to the same `_` local name.
        with self.assertRaisesRegex(contract.ContractError, 'phase'):
            self.update(self.lease(phase='reserved'))
        with self.assertRaisesRegex(contract.ContractError, 'Unknown'):
            self.update(self.lease(), 'not-an-event')


if __name__ == '__main__':
    unittest.main()
