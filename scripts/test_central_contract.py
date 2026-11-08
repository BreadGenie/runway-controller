"""Exercise the actual lease contract shipped in the Central integration patch."""

import importlib.util
import json
from pathlib import Path
import unittest
import sys
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location(
    'central_contract', Path(__file__).resolve().parents[1] / 'integrations/central/contract_harness.py'
)
contract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(contract)


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

    def test_pool_placement_rules_are_optional_and_require_an_array_of_objects(self):
        namespace = {'frappe': SimpleNamespace(parse_json=json.loads, throw=contract.reject),
                     '_': lambda value: value}
        contract.load_functions('central/infrastructure/runway.py', ['pool_placement_rules'], namespace)
        read = namespace['pool_placement_rules']
        self.assertEqual(read({}), [])
        rules = [{'resource': 'metal_server', 'operator': 'has', 'tags': {'lab': 'blr'}}]
        self.assertEqual(read({'placement_rules': json.dumps(rules)}), rules)
        for invalid in ('{}', 'null', '[1]', '["host"]'):
            with self.subTest(invalid=invalid), self.assertRaises(contract.ContractError):
                read({'placement_rules': invalid})

    @unittest.skipIf(sys.version_info < (3, 14), 'pilot integration requires Python 3.14')
    def test_runner_create_forwards_placement_without_changing_default(self):
        sent = []
        namespace = {'frappe': SimpleNamespace(db=SimpleNamespace(commit=lambda: None)),
                     'pool_placement_rules': lambda pool: json.loads(pool.get('placement_rules') or '[]'),
                     'client_for_pool': lambda pool: SimpleNamespace(
                         create_vm=lambda payload: sent.append(payload.copy()) or {'id': 'vm'}),
                     'save_internal': lambda lease: None,
                     'AtlasRejected': RuntimeError, 'AtlasRequestUncertain': OSError,
                     'AtlasConnectionError': ConnectionError}
        contract.load_functions('central/integrations/runway.py', ['create_vm'], namespace)
        pool = contract.Document(image_id='image', cpu_millicores=1000, memory_mib=1024,
                                 disk_mib=4096, ssh_public_key='public', name='pool', team='team')
        lease = contract.Document(runner_name='runner', name='lease', reload=lambda: None)
        namespace['create_vm'](pool, lease)
        self.assertNotIn('placement_rules', sent[-1])
        rules = [{'resource': 'metal_server', 'operator': 'has', 'tags': {'lab': 'blr'}}]
        pool.placement_rules = json.dumps(rules)
        namespace['create_vm'](pool, lease)
        self.assertEqual(sent[-1]['placement_rules'], rules)
        self.assertEqual(lease.vm_id, 'vm')

    def test_runner_identity_storage_accepts_github_ids_above_signed_int32(self):
        schema = json.loads(contract.added_file(
            'central/infrastructure/doctype/runner_lease/runner_lease.json'))
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
