#!/bin/bash
# Deployment script for Infinite Streaming WordPress site

set -e

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# Configuration
NAMESPACE="default"
WORDPRESS_POD=""
MYSQL_POD=""

echo -e "${BLUE}=== Infinite Streaming WordPress Deployment ===${NC}"
echo ""

# Function to check if kubectl is available
check_kubectl() {
    if ! command -v kubectl &> /dev/null; then
        echo -e "${RED}Error: kubectl is not installed or not in PATH${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ kubectl found${NC}"
}

# Function to check k3s connectivity
check_k3s() {
    if ! kubectl cluster-info &> /dev/null; then
        echo -e "${RED}Error: Cannot connect to k3s cluster${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ Connected to k3s cluster${NC}"
}

# Function to create host directories
create_directories() {
    echo -e "${BLUE}Creating host directories...${NC}"
    
    # Note: This assumes the script is run on the k3s host (lenovo)
    # If running remotely, you'll need to SSH in
    
    sudo mkdir -p /home/jonathanoliver/wordpress/mysql
    sudo mkdir -p /home/jonathanoliver/wordpress/wp-content
    sudo chown -R $(id -u):$(id -g) /home/jonathanoliver/wordpress
    
    echo -e "${GREEN}✓ Directories created${NC}"
}

# Function to deploy WordPress
deploy_wordpress() {
    echo -e "${BLUE}Deploying WordPress to k3s...${NC}"
    
    kubectl apply -f k8s-wordpress.yaml
    
    echo -e "${GREEN}✓ WordPress deployment applied${NC}"
    echo ""
    echo -e "${YELLOW}Waiting for pods to start (this may take a minute)...${NC}"
    
    # Wait for MySQL pod
    kubectl wait --for=condition=ready pod -l app=wordpress,tier=mysql --timeout=300s
    echo -e "${GREEN}✓ MySQL pod is ready${NC}"
    
    # Wait for WordPress pod
    kubectl wait --for=condition=ready pod -l app=wordpress,tier=frontend --timeout=300s
    echo -e "${GREEN}✓ WordPress pod is ready${NC}"
}

# Function to get pod names
get_pod_names() {
    WORDPRESS_POD=$(kubectl get pods -l app=wordpress,tier=frontend -o jsonpath='{.items[0].metadata.name}')
    MYSQL_POD=$(kubectl get pods -l app=wordpress,tier=mysql -o jsonpath='{.items[0].metadata.name}')
    
    echo -e "${GREEN}✓ WordPress pod: ${WORDPRESS_POD}${NC}"
    echo -e "${GREEN}✓ MySQL pod: ${MYSQL_POD}${NC}"
}

# Function to show access information
show_access_info() {
    echo ""
    echo -e "${GREEN}=== Deployment Complete! ===${NC}"
    echo ""
    echo -e "${BLUE}Access your WordPress site at:${NC}"
    echo -e "  • http://lenovo.local:30080"
    echo -e "  • http://192.168.0.189:30080"
    echo ""
    echo -e "${BLUE}Next steps:${NC}"
    echo -e "  1. Open your browser and go to http://lenovo.local:30080"
    echo -e "  2. Complete the WordPress installation wizard"
    echo -e "  3. Follow the instructions in wordpress-content/README.md"
    echo -e "  4. Upload screenshots and configure the homepage"
    echo ""
    echo -e "${YELLOW}Useful commands:${NC}"
    echo -e "  • View WordPress logs: kubectl logs -f ${WORDPRESS_POD}"
    echo -e "  • View MySQL logs: kubectl logs -f ${MYSQL_POD}"
    echo -e "  • Access WordPress shell: kubectl exec -it ${WORDPRESS_POD} -- bash"
    echo -e "  • Check pod status: kubectl get pods"
    echo ""
}

# Function to check if already deployed
check_existing_deployment() {
    if kubectl get deployment wordpress &> /dev/null; then
        echo -e "${YELLOW}WordPress deployment already exists.${NC}"
        read -p "Do you want to redeploy? (y/N): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            echo -e "${BLUE}Skipping deployment. Checking pod status...${NC}"
            get_pod_names
            show_access_info
            exit 0
        else
            echo -e "${YELLOW}Deleting existing deployment...${NC}"
            kubectl delete -f k8s-wordpress.yaml || true
            echo -e "${YELLOW}Waiting for cleanup...${NC}"
            sleep 10
        fi
    fi
}

# Function to show help
show_help() {
    cat << EOF
Infinite Streaming WordPress Deployment Script

Usage: $0 [OPTIONS]

OPTIONS:
    -h, --help              Show this help message
    -d, --deploy            Deploy WordPress (default action)
    -s, --status            Show deployment status
    -r, --restart           Restart WordPress deployment
    -c, --cleanup           Remove WordPress deployment
    -l, --logs              Show WordPress logs
    --skip-dirs             Skip directory creation (if already exists)

Examples:
    $0                      # Deploy WordPress
    $0 --status             # Check deployment status
    $0 --restart            # Restart WordPress
    $0 --cleanup            # Remove deployment
    $0 --logs               # View logs

EOF
}

# Function to show status
show_status() {
    echo -e "${BLUE}=== WordPress Deployment Status ===${NC}"
    echo ""
    
    echo -e "${BLUE}Pods:${NC}"
    kubectl get pods -l app=wordpress
    echo ""
    
    echo -e "${BLUE}Services:${NC}"
    kubectl get svc -l app=wordpress
    echo ""
    
    echo -e "${BLUE}PersistentVolumeClaims:${NC}"
    kubectl get pvc
    echo ""
    
    if kubectl get deployment wordpress &> /dev/null; then
        get_pod_names
        echo ""
        echo -e "${GREEN}WordPress is deployed${NC}"
        echo -e "Access at: http://lenovo.local:30080"
    else
        echo -e "${YELLOW}WordPress is not deployed${NC}"
    fi
}

# Function to restart deployment
restart_deployment() {
    echo -e "${BLUE}Restarting WordPress deployment...${NC}"
    kubectl rollout restart deployment wordpress
    kubectl rollout restart deployment wordpress-mysql
    echo -e "${GREEN}✓ Restart initiated${NC}"
    
    kubectl rollout status deployment wordpress
    kubectl rollout status deployment wordpress-mysql
    echo -e "${GREEN}✓ Deployments restarted${NC}"
}

# Function to cleanup
cleanup_deployment() {
    echo -e "${RED}WARNING: This will delete the WordPress deployment${NC}"
    echo -e "${YELLOW}Note: This will NOT delete the persistent data on disk${NC}"
    read -p "Are you sure? (y/N): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo -e "${BLUE}Removing deployment...${NC}"
        kubectl delete -f k8s-wordpress.yaml || true
        echo -e "${GREEN}✓ Deployment removed${NC}"
        echo ""
        echo -e "${YELLOW}Persistent data remains at:${NC}"
        echo -e "  • /home/jonathanoliver/wordpress/mysql"
        echo -e "  • /home/jonathanoliver/wordpress/wp-content"
    fi
}

# Function to show logs
show_logs() {
    get_pod_names
    echo -e "${BLUE}Showing WordPress logs (press Ctrl+C to exit)...${NC}"
    kubectl logs -f ${WORDPRESS_POD}
}

# Main script
main() {
    case "${1:-}" in
        -h|--help)
            show_help
            exit 0
            ;;
        -s|--status)
            check_kubectl
            check_k3s
            show_status
            exit 0
            ;;
        -r|--restart)
            check_kubectl
            check_k3s
            restart_deployment
            exit 0
            ;;
        -c|--cleanup)
            check_kubectl
            check_k3s
            cleanup_deployment
            exit 0
            ;;
        -l|--logs)
            check_kubectl
            check_k3s
            show_logs
            exit 0
            ;;
        -d|--deploy|"")
            # Default action: deploy
            check_kubectl
            check_k3s
            check_existing_deployment
            
            if [[ "${2:-}" != "--skip-dirs" ]]; then
                create_directories
            fi
            
            deploy_wordpress
            get_pod_names
            show_access_info
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            show_help
            exit 1
            ;;
    esac
}

# Run main function
main "$@"
