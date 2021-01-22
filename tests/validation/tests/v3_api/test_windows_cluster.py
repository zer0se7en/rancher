from .common import TEST_IMAGE
from .common import TEST_IMAGE_NGINX
from .common import TEST_IMAGE_OS_BASE
from .common import cluster_cleanup
from .common import get_user_client
from .common import random_test_name
from .test_rke_cluster_provisioning import create_custom_host_from_nodes
from .test_rke_cluster_provisioning import HOST_NAME

from lib.aws import AmazonWebServices, AWS_DEFAULT_AMI, AWS_DEFAULT_USER


def provision_windows_nodes():
    node_roles_linux = [["controlplane"], ["etcd"], ["worker"]]
    node_roles_windows = [["worker"], ["worker"], ["worker"]]

    win_nodes = \
        AmazonWebServices().create_multiple_nodes(
            len(node_roles_windows), random_test_name(HOST_NAME))

    linux_nodes = \
        AmazonWebServices().create_multiple_nodes(
            len(node_roles_linux), random_test_name(HOST_NAME),
            ami=AWS_DEFAULT_AMI, ssh_user=AWS_DEFAULT_USER)

    nodes = linux_nodes + win_nodes
    node_roles = node_roles_linux + node_roles_windows

    for node in win_nodes:
        pull_images(node)

    return nodes, node_roles


def test_windows_provisioning_vxlan():
    nodes, node_roles = provision_windows_nodes()

    cluster, nodes = create_custom_host_from_nodes(nodes, node_roles,
                                                   random_cluster_name=True,
                                                   windows=True,
                                                   windows_flannel_backend='vxlan')

    cluster_cleanup(get_user_client(), cluster, nodes)


def test_windows_provisioning_gw_host():
    nodes, node_roles = provision_windows_nodes()

    for node in nodes:
        AmazonWebServices().disable_source_dest_check(node.provider_node_id)

    cluster, nodes = create_custom_host_from_nodes(nodes, node_roles,
                                                   random_cluster_name=True,
                                                   windows=True,
                                                   windows_flannel_backend='host-gw')

    cluster_cleanup(get_user_client(), cluster, nodes)


def pull_images(node):
    print("Pulling images on node: " + node.host_name)
    pull_result = node.execute_command("docker pull " + TEST_IMAGE
                                       + " && " +
                                       "docker pull " + TEST_IMAGE_NGINX
                                       + " && " +
                                       "docker pull " + TEST_IMAGE_OS_BASE)
    print(pull_result)
