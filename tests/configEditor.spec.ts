import { test, expect } from '@grafana/plugin-e2e';
import { HotdataDataSourceOptions, HotdataSecureJsonData } from '../src/types';

test('smoke: should render config editor', async ({ createDataSourceConfigPage, readProvisionedDataSource, page }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await createDataSourceConfigPage({ type: ds.type });
  await expect(page.getByLabel('API URL')).toBeVisible();
  await expect(page.getByLabel('Workspace ID')).toBeVisible();
});

// Requires the mock API (`go run ./pkg/cmd/mockapi`); the provisioning file's
// values are env placeholders on disk, so the mock's coordinates are inlined.
test('"Save & test" should be successful when configuration is valid', async ({
  createDataSourceConfigPage,
  readProvisionedDataSource,
  page,
}) => {
  const ds = await readProvisionedDataSource<HotdataDataSourceOptions, HotdataSecureJsonData>({
    fileName: 'datasources.yml',
  });
  const configPage = await createDataSourceConfigPage({ type: ds.type });
  await page.getByRole('textbox', { name: 'API URL' }).fill('http://host.docker.internal:8999');
  await page.getByRole('textbox', { name: 'API key' }).fill('hd_mock');
  await page.getByRole('textbox', { name: 'Workspace ID' }).fill('workmock');
  await page.getByRole('textbox', { name: 'Default database ID' }).fill('dbmock');
  await expect(configPage.saveAndTest()).toBeOK();
});

test('"Save & test" should fail when the API key is missing', async ({
  createDataSourceConfigPage,
  readProvisionedDataSource,
  page,
}) => {
  const ds = await readProvisionedDataSource<HotdataDataSourceOptions, HotdataSecureJsonData>({
    fileName: 'datasources.yml',
  });
  const configPage = await createDataSourceConfigPage({ type: ds.type });
  await page.getByRole('textbox', { name: 'Workspace ID' }).fill(ds.jsonData.workspaceId ?? 'workmock');
  await expect(configPage.saveAndTest()).not.toBeOK();
  await expect(configPage).toHaveAlert('error', { hasText: 'API key is missing' });
});
