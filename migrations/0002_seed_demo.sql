-- Development ONLY. Never apply automatically in a production environment.
INSERT INTO users(id,oidc_subject,display_name,status) VALUES
 ('00000000-0000-4000-8000-000000000001','dev:alice','Alice · Engineering','active'),
 ('00000000-0000-4000-8000-000000000002','dev:bob','Bob · Marketing','active'),
 ('00000000-0000-4000-8000-000000000003','dev:disabled','Disabled Staff','disabled')
ON CONFLICT(id) DO NOTHING;
