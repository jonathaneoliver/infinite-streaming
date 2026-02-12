# InfiniteStreamer.com WordPress Site

This directory contains the WordPress deployment configuration for infinitestreamer.com, designed to run on your k3s server (lenovo).

## Prerequisites

- k3s cluster running on lenovo
- kubectl configured to access your k3s cluster
- Host directories for persistent storage

## Quick Start

### 1. Create Host Directories

On your lenovo server, create the directories for persistent storage:

```bash
sudo mkdir -p /home/jonathanoliver/wordpress/mysql
sudo mkdir -p /home/jonathanoliver/wordpress/wp-content
sudo chown -R $(id -u):$(id -g) /home/jonathanoliver/wordpress
```

### 2. Deploy WordPress

```bash
kubectl apply -f k8s-wordpress.yaml
```

### 3. Wait for Pods to Start

```bash
kubectl get pods -w
```

Wait until both `wordpress-mysql-*` and `wordpress-*` pods are in Running state.

### 4. Access WordPress

Open your browser and navigate to:
```
http://lenovo.local:30080
```

Or using the IP address:
```
http://192.168.0.189:30080
```

### 5. Complete WordPress Installation

1. Select your language
2. Click "Continue"
3. Fill in the site information:
   - **Site Title**: Infinite Streaming
   - **Username**: admin (or your preferred username)
   - **Password**: (use a strong password)
   - **Your Email**: your-email@example.com
4. Click "Install WordPress"

### 6. Configure Homepage Content

After installation, follow these steps to create the homepage:

#### Option A: Manual Setup (Recommended for Full Control)

1. Log in to WordPress admin at `http://lenovo.local:30080/wp-admin`
2. Go to **Pages > Add New**
3. Create a new page titled "Home"
4. Use the content from `wordpress-content/homepage-content.html` (see below)
5. Publish the page
6. Go to **Settings > Reading**
7. Select "A static page" and choose "Home" as your homepage
8. Save changes

#### Option B: Use the Initialization Script

If you prefer automated setup, you can use the provided initialization script after WordPress is installed:

```bash
# Copy the initialization script to the WordPress pod
kubectl cp wordpress-content/init-homepage.sh wordpress-POD-NAME:/tmp/

# Execute the script inside the pod
kubectl exec -it wordpress-POD-NAME -- bash /tmp/init-homepage.sh
```

Replace `wordpress-POD-NAME` with your actual WordPress pod name (find it with `kubectl get pods`).

### 7. Add Screenshots

After setting up the homepage:

1. Log in to WordPress admin
2. Go to **Media > Add New**
3. Upload all images from the `screenshots/` directory:
   - dashboard.png
   - encoding-jobs.png
   - live-offset.png
   - mosaic.png
   - playback.png
   - source-library.png
   - testing-player.png
   - upload-content.png

4. Edit your homepage and insert images where marked in the content

### 8. Configure Theme (Optional)

For a minimal, professional look:

1. Go to **Appearance > Themes**
2. Install and activate "Astra" (free, lightweight, and fast)
3. Or use any minimalist theme like "GeneratePress" or "Kadence"

For more customization:
1. Go to **Appearance > Customize**
2. Adjust colors, fonts, and layout
3. Make it responsive and mobile-friendly

## Domain Configuration

To use infinitestreamer.com:

1. Update your DNS settings at GoDaddy:
   - Create an A record pointing to your lenovo server's public IP
   - Or create a CNAME if using a proxy service

2. Update WordPress site URL:
   ```bash
   kubectl exec -it wordpress-POD-NAME -- wp option update siteurl 'http://infinitestreamer.com' --allow-root
   kubectl exec -it wordpress-POD-NAME -- wp option update home 'http://infinitestreamer.com' --allow-root
   ```

3. For HTTPS (recommended for production):
   - Set up a reverse proxy with SSL (nginx, traefik, or k3s ingress)
   - Use Let's Encrypt for free SSL certificates
   - Update WordPress URLs to https://infinitestreamer.com

## Maintenance

### Backup

```bash
# Backup MySQL database
kubectl exec wordpress-mysql-POD-NAME -- mysqldump -u wordpress -pWordPressPass123! wordpress > wordpress-backup.sql

# Backup WordPress files
kubectl cp wordpress-POD-NAME:/var/www/html ./wordpress-backup
```

### Update WordPress

```bash
# Update from WordPress admin or use WP-CLI:
kubectl exec -it wordpress-POD-NAME -- wp core update --allow-root
kubectl exec -it wordpress-POD-NAME -- wp plugin update --all --allow-root
kubectl exec -it wordpress-POD-NAME -- wp theme update --all --allow-root
```

### Scale or Restart

```bash
# Restart WordPress
kubectl rollout restart deployment wordpress

# Restart MySQL
kubectl rollout restart deployment wordpress-mysql

# Delete and redeploy
kubectl delete -f k8s-wordpress.yaml
kubectl apply -f k8s-wordpress.yaml
```

## Troubleshooting

### Check pod logs
```bash
kubectl logs -f wordpress-POD-NAME
kubectl logs -f wordpress-mysql-POD-NAME
```

### Check pod status
```bash
kubectl describe pod wordpress-POD-NAME
kubectl describe pod wordpress-mysql-POD-NAME
```

### Access WordPress container
```bash
kubectl exec -it wordpress-POD-NAME -- bash
```

### Common Issues

1. **Pods not starting**: Check PersistentVolume paths exist on the host
2. **Database connection error**: Wait for MySQL pod to be fully ready
3. **Port already in use**: Change nodePort in k8s-wordpress.yaml if 30080 is taken
4. **Permission denied on volumes**: Check directory ownership on host

## Security Notes

**Important for Production:**

1. Change default passwords in `k8s-wordpress.yaml` before deploying
2. Use Kubernetes secrets properly (don't commit passwords to git)
3. Set up SSL/TLS for HTTPS
4. Keep WordPress, plugins, and themes updated
5. Use strong admin passwords
6. Install security plugins like Wordfence
7. Regular backups

## Customization

The homepage content is designed to be easily editable through the WordPress editor. You can:

- Update text and descriptions
- Add/remove features
- Change layout
- Update screenshots
- Add new sections
- Modify calls-to-action

All changes can be made through the WordPress admin interface without touching code.

## Support

For issues with:
- **Infinite Streaming**: See [GitHub repository](https://github.com/jonathaneoliver/infinite-streaming)
- **WordPress deployment**: Check Kubernetes logs and this README
- **Domain/DNS**: Consult GoDaddy support or your DNS provider
