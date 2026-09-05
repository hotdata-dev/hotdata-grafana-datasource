import { test, expect } from '@grafana/plugin-e2e';

// The SQL editor is Monaco; its textarea carries Monaco's own accessible name.
const sqlEditor = /Editor content/;

test('smoke: should render query editor', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  await expect(panelEditPage.getQueryEditorRow('A').getByRole('textbox', { name: sqlEditor })).toBeVisible();
});

// Requires the mock API (`go run ./pkg/cmd/mockapi`) or a live workspace
// behind the provisioned datasource.
test('table query should return the mock series', async ({ panelEditPage, readProvisionedDataSource, page }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  await panelEditPage.getQueryEditorRow('A').getByRole('textbox', { name: sqlEditor }).click();
  await page.keyboard.type('SELECT ts, service, value FROM metrics');
  // Dismiss Monaco's completion popup so it cannot swallow the refresh click.
  await page.keyboard.press('Escape');
  await panelEditPage.setVisualization('Table');
  await expect(panelEditPage.refreshPanel()).toBeOK();
  await expect(panelEditPage.panel.data).toContainText(['api']);
});
