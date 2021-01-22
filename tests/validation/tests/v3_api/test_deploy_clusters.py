from .common import *  # NOQA
from .test_aks_cluster import get_aks_version, create_and_validate_aks_cluster
from .test_eks_cluster import EKS_K8S_VERSIONS, create_and_validate_eks_cluster
from .test_gke_cluster import get_gke_config, \
    create_and_validate_gke_cluster, get_gke_version_credentials
from .test_rke_cluster_provisioning import create_and_validate_custom_host
from .test_import_cluster import create_and_validate_import_cluster

env_details = "env.RANCHER_CLUSTER_NAMES='"
cluster_details = {"rke": {}, "rke_import": {},
                   "eks": {}, "aks": {}, "gke": {}}

if_not_auto_deploy_rke = pytest.mark.skipif(
    ast.literal_eval(
        os.environ.get(
            'RANCHER_TEST_DEPLOY_RKE', "False")) is False,
    reason='auto deploy RKE tests are skipped')
if_not_auto_deploy_eks = pytest.mark.skipif(
    ast.literal_eval(
        os.environ.get(
            'RANCHER_TEST_DEPLOY_EKS', "False")) is False,
    reason='auto deploy EKS tests are skipped')
if_not_auto_deploy_gke = pytest.mark.skipif(
    ast.literal_eval(
        os.environ.get(
            'RANCHER_TEST_DEPLOY_GKE', "False")) is False,
    reason='auto deploy GKE tests are skipped')
if_not_auto_deploy_aks = pytest.mark.skipif(
    ast.literal_eval(
        os.environ.get(
            'RANCHER_TEST_DEPLOY_AKS', "False")) is False,
    reason='auto deploy AKS tests are skipped')
if_not_auto_deploy_rke_import = pytest.mark.skipif(
    ast.literal_eval(
        os.environ.get(
            'RANCHER_TEST_DEPLOY_RKE_IMPORT', "False")) is False,
    reason='auto deploy RKE import tests are skipped')


@if_not_auto_deploy_rke
def test_deploy_rke():
    print("Deploying RKE Clusters")
    global env_details
    global cluster_details

    rancher_version = get_setting_value_by_name('server-version')
    if str(rancher_version).startswith('v2.2'):
        k8s_v = get_setting_value_by_name('k8s-version-to-images')
        default_k8s_versions = json.loads(k8s_v).keys()
    else:
        k8s_v = get_setting_value_by_name('k8s-versions-current')
        default_k8s_versions = k8s_v.split(",")

    # Create clusters
    for k8s_version in default_k8s_versions:
        if env_details != "env.RANCHER_CLUSTER_NAMES='":
            env_details += ","
        print("Deploying RKE Cluster using kubernetes version {}".format(
            k8s_version))
        node_roles = [["controlplane"], ["etcd"],
                      ["worker"], ["worker"], ["worker"]]
        cluster, aws_nodes = create_and_validate_custom_host(
            node_roles, random_cluster_name=True, version=k8s_version)
        env_details += cluster.name
        print("Successfully deployed {} with kubernetes version {}".format(
            cluster.name, k8s_version))
        cluster_details["rke"][cluster.name] = k8s_version


@if_not_auto_deploy_rke_import
def test_deploy_rke_import():
    print("Deploying RKE import Clusters")
    global env_details
    global cluster_details
    errors = []
    # get rke k8s versions
    rkecommand = "rke config --list-version -a"
    result = run_command_with_stderr(rkecommand)
    result = result.decode('ascii')
    import_k8s_versions = result.split('\n')
    import_k8s_versions = list(filter(None, import_k8s_versions))
    print(import_k8s_versions)

    for version in import_k8s_versions:
        if env_details != "env.RANCHER_CLUSTER_NAMES='":
            env_details += ","
        try:
            print("Deploying RKE import Cluster using kubernetes version {}".
                  format(version))
            client, cluster, \
                aws_nodes = \
                create_and_validate_import_cluster(version, supportmatrix=True)
            env_details += cluster.name
            cluster_details["rke_import"][cluster.name] = version
        except Exception as e:
            errors.append(e)


@if_not_auto_deploy_eks
def test_deploy_eks():
    print("Deploying EKS Clusters")
    global env_details
    errors = []
    if len(EKS_K8S_VERSIONS) > 1:
        k8s_versions = [EKS_K8S_VERSIONS[0], EKS_K8S_VERSIONS[-1]]
    else:
        k8s_versions = [EKS_K8S_VERSIONS[0]]

    for version in k8s_versions:
        if env_details != "env.RANCHER_CLUSTER_NAMES='":
            env_details += ","
        try:
            print("Deploying EKS Cluster using kubernetes version {}".format(
                version))
            client, cluster = create_and_validate_eks_cluster(version)
            env_details += cluster.name
            cluster_details["eks"][cluster.name] = version
        except Exception as e:
            errors.append(e)

    assert not errors


@if_not_auto_deploy_gke
def test_deploy_gke():
    print("Deploying GKE Clusters")
    global env_details
    errors = []

    gke_versions, creds = get_gke_version_credentials(multiple_versions=True)

    for i, version in enumerate(gke_versions, start=1):
        c_name = "test-auto-gke-{}".format(i)
        if env_details != "env.RANCHER_CLUSTER_NAMES='":
            env_details += ","
        try:
            print("Deploying GKE Cluster using kubernetes version {}".format(
                version))
            client, cluster = create_and_validate_gke_cluster(c_name,
                                                              version, creds)
            env_details += cluster.name
            cluster_details["gke"][cluster.name] = version
        except Exception as e:
            errors.append(e)

    assert not errors


@if_not_auto_deploy_aks
def test_deploy_aks():
    print("Deploying AKS Clusters")
    global env_details
    errors = []

    aks_versions = get_aks_version(multiple_versions=True)

    for version in aks_versions:
        if env_details != "env.RANCHER_CLUSTER_NAMES='":
            env_details += ","
        try:
            print("Deploying AKS Cluster using kubernetes version {}".format(
                version))
            client, cluster = create_and_validate_aks_cluster(version)
            env_details += cluster.name
            cluster_details["aks"][cluster.name] = version
        except Exception as e:
            errors.append(e)

    assert not errors


@pytest.fixture(scope='module', autouse="True")
def set_data(request):
    if UPDATE_KDM is True:
        update_and_validate_kdm(KDM_URL)

    def fin():
        global env_details
        global cluster_details
        env_details += "'"
        print("\n{}".format(env_details))
        print("\n Cluster Details")
        for cluster_type in cluster_details:
            print(cluster_type + ": " + str(cluster_details[cluster_type]))
        for cluster_type in cluster_details:
            for cluster_name in cluster_details[cluster_type]:
                print(cluster_type + " --> " +
                      str(cluster_details[cluster_type][cluster_name]))
        create_config_file(env_details)

    request.addfinalizer(fin)
